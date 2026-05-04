package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

func newJob(r *http.Request, w http.ResponseWriter, logger *customLogger) (err error) {
	jobID := uuid.New().String()

	// Check if client has broken redirect behavior, like the android app.
	// Redirect support is necessary as file processing can take a long time and the client risks a timeout on the http request.
	// Prefer an explicit `Accept` header indicating an HTML-capable client (browser).
	// Fallback to a blacklist for known clients with broken redirect behavior.
	acceptHeader := r.Header.Get("Accept")
	clientFollowsRedirects := strings.Contains(acceptHeader, "text/html")

	brokenRedirectUserAgents := []string{"Dart/", "Dalvik/", "Immich"}
	for _, userAgent := range brokenRedirectUserAgents {
		if strings.HasPrefix(r.UserAgent(), userAgent) {
			clientFollowsRedirects = false
			break
		}
	}
	if !clientFollowsRedirects {
		logger = newCustomLogger(logger, "client with broken redirects: ")
	}

	jobLogger := newCustomLogger(logger, fmt.Sprintf("job %s: ", jobID))

	jobLogger.Printf("intercepting upload")

	formFile, formFileHeader, err := r.FormFile(filterFormKey)
	if err != nil {
		err = fmt.Errorf("unable to read file in key %s from uploaded form data: %w", filterFormKey, err)
		return
	}
	defer formFile.Close()

	jobLogger.Printf("uploaded %s %s", formFileHeader.Filename, humanReadableSize(formFileHeader.Size))

	// Create the channels for this job
	jobRespCh := make(chan *http.Response)
	jobDoneCh := make(chan struct{}, 1)
	jobsMu.Lock()
	jobChannels[jobID] = jobRespCh
	jobChannelsComplete[jobID] = jobDoneCh
	jobsMu.Unlock()

	cleanup1 := func() {
		jobsMu.Lock()
		delete(jobChannels, jobID)
		delete(jobChannelsComplete, jobID)
		jobsMu.Unlock()
		close(jobRespCh)
	}

	// Redirect the user to the job wait page
	if clientFollowsRedirects {
		http.Redirect(w, r, fmt.Sprintf("/_immich-upload-optimizer/wait?job=%s", jobID), http.StatusTemporaryRedirect)
		w.(http.Flusher).Flush()
	}

	// Continue processing the file
	tp, err := NewTaskProcessorFromMultipart(formFile, formFileHeader)
	if err != nil {
		defer cleanup1()
		err = fmt.Errorf("unable to create task processor: %w", err)
		if !clientFollowsRedirects {
			http.Error(w, "failed to process file, view logs for more info", http.StatusInternalServerError)
		}
		return
	}

	tp.SetLogger(jobLogger)

	cleanup2 := func() {
		tp.Close()
		cleanup1()
	}

	err = tp.Process(config.Tasks)
	if err != nil {
		defer cleanup2()
		err = fmt.Errorf("failed to process file in job %s: %v", jobID, err.Error())
		if !clientFollowsRedirects {
			http.Error(w, "failed to process file, view logs for more info", http.StatusInternalServerError)
		}
		return
	}

	replace := tp.OriginalSize > tp.ProcessedSize

	// Create the form data to be sent upstream
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	for key, values := range r.MultipartForm.Value {
		for _, value := range values {
			err = writer.WriteField(key, value)
			if err != nil {
				defer cleanup2()
				err = fmt.Errorf("unable to create form data to be sent upstream: %w", err)
				return
			}
		}
	}

	uploadFilename := tp.OriginalFilename
	uploadFile := tp.OriginalFile
	if replace {
		uploadFilename = tp.ProcessedFilename
		uploadFile = tp.ProcessedFile
	}

	part, err := writer.CreateFormFile(filterFormKey, uploadFilename)
	if err != nil {
		defer cleanup2()
		err = fmt.Errorf("unable to create file form field to be sent upstream: %w", err)
		return
	}

	_, err = tp.OriginalFile.Seek(0, io.SeekStart)
	if err != nil {
		defer cleanup2()
		err = fmt.Errorf("unable to seek beginning of temp file: %w", err)
		return
	}

	_, err = io.Copy(part, uploadFile)
	if err != nil {
		defer cleanup2()
		err = fmt.Errorf("unable to write file in form field to be sent upstream: %w", err)
		return
	}

	err = writer.Close()
	if err != nil {
		defer cleanup2()
		err = fmt.Errorf("unable to finish form data to be sent upstream: %w", err)
		return
	}

	// Send the request to the upstream server
	destination := *remote
	destination.Path = path.Join(destination.Path, r.URL.Path)
	req, err := http.NewRequest("POST", destination.String(), &buffer)
	if err != nil {
		defer cleanup2()
		err = fmt.Errorf("unable to create POST request to upstream: %w", err)
		return
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		defer cleanup2()
		err = fmt.Errorf("unable to POST to upstream: %w", err)
		return
	}
	//defer resp.Body.Close() // Done bellow to allow original http request to be closed first

	// Log the result
	action := "file NOT replaced"
	if replace {
		action = "file replaced"
	}

	jobLogger.Printf("%s: \"%s\" %s optimized to \"%s\" %s", action, tp.OriginalFilename, humanReadableSize(tp.OriginalSize), tp.ProcessedFilename, humanReadableSize(tp.ProcessedSize))

	cleanup3 := func() {
		resp.Body.Close()
		cleanup2()
	}

	if !clientFollowsRedirects {
		defer cleanup3()

		w.WriteHeader(resp.StatusCode)
		_, err = io.Copy(w, resp.Body)
		if err != nil {
			err = fmt.Errorf("unable to forward response back to client directly: %v", err)
		} else {
			jobLogger.Printf("response sent back to client directly")
		}
		return
	}

	// Allow the function to return so the client request ends
	go func() {
		defer cleanup3()

		// Send the response back to the client via the wait page
		select {
		case jobRespCh <- resp:
			// Wait for the response to be sent to the client before cleaning up or timeout.
			// This is to avoid all the deferred functions to run before the response is fully sent.
			select {
			case <-jobDoneCh:
				jobLogger.Printf("response sent to client")
			case <-time.After(10 * time.Second):
				jobLogger.Printf("timeout before response was fully sent to client")
			}
		case <-time.After(10 * time.Second):
			jobLogger.Printf("timeout while waiting for client to ask for a response on the redirect wait page, redirect was not followed by the client.")
		}
	}()

	return nil
}

func continueJob(r *http.Request, w http.ResponseWriter, requestLogger *customLogger) {
	jobID := r.URL.Query().Get("job")
	jobsMu.Lock()
	jobChannel, exists := jobChannels[jobID]
	jobCompleteCh, _ := jobChannelsComplete[jobID]
	jobsMu.Unlock()
	if jobID == "" || !exists {
		http.Error(w, "job not found", http.StatusBadRequest)
		return
	}

	jobLogger := newCustomLogger(requestLogger, fmt.Sprintf("job %s: ", jobID))

	// Parse the form data again as not to leave the POST hanging, some proxies like cloudflare will error without this.
	err := r.ParseMultipartForm(10 << 20) // store up to 10 MB in memory to prevent disk writes
	if err != nil {
		// ignore as we already have the data
	}

	// 55s to avoid browser timeout
	safeClientTimeout := time.Duration(55) * time.Second

	select {
	case resp, ok := <-jobChannel:
		if !ok {
			msg := "job channel closed unexpectedly"
			http.Error(w, msg, http.StatusInternalServerError)
			requestLogger.Printf(msg)
			return
		}
		// @TODO
		// It prevents cookie headers from being forwarded, potentially leaking them,
		// so when done a job should only match if it belongs to the corresponding session.
		// for key, values := range resp.Header {
		// 	for _, value := range values {
		// 		w.Header().Add(key, value)
		// 	}
		// }
		w.WriteHeader(resp.StatusCode)
		_, err := io.Copy(w, resp.Body)
		if err != nil {
			jobLogger.Printf("unable to forward response back to client: %v", err)
		}
		jobCompleteCh <- struct{}{}
	case <-time.After(safeClientTimeout):
		http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
		jobLogger.Printf("still running, sending redirect to avoid client timeout: %s", r.URL)
	}
}
