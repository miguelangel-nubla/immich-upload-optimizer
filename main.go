package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"text/template"

	"github.com/spf13/viper"
)

var remote *url.URL
var convertCMDTemplate *template.Template
var (
	currentJobID          = 0
	maxConcurrentRequests = 10
	semaphore             = make(chan struct{}, maxConcurrentRequests)
)

var upstreamURL string
var listenAddr string
var convertCMD string
var extensionWhitelist string
var filterPath string
var filterFormKey string

func init() {
	viper.SetEnvPrefix("iuo")
	viper.AutomaticEnv()
	viper.BindEnv("upstream")
	viper.BindEnv("listen")
	viper.BindEnv("convert_cmd")
	viper.BindEnv("filter_path")
	viper.BindEnv("filter_form_key")

	viper.SetDefault("upstream", "")
	viper.SetDefault("listen", ":2283")
	viper.SetDefault("convert_cmd", "caesiumclt --keep-dates --exif --quality=0 --output={{.folder}} {{.folder}}/{{.name}}.{{.extension}}")
	viper.SetDefault("extension_whitelist", "jpeg,jpg,png,tiff,tif,webp,gif")
	viper.SetDefault("filter_path", "/api/assets")
	viper.SetDefault("filter_form_key", "assetData")

	flag.StringVar(&upstreamURL, "upstream", viper.GetString("upstream"), "Upstream URL. Example: http://immich-server:2283")
	flag.StringVar(&listenAddr, "listen", viper.GetString("listen"), "Listening address")
	flag.StringVar(&convertCMD, "convert_cmd",
		viper.GetString("convert_cmd"),
		"Command to apply to convert files, available placeholders: folder, name, extension. "+
			"The original file is in a temp folder by itself. "+
			"This utility will read the converted file from the same folder, so you need to delete or overwrite the original.")
	flag.StringVar(&extensionWhitelist, "extension_whitelist", viper.GetString("extension_whitelist"), "Comma-separated list of file extensions to process. Defaults to the supported extensions of the bundled converter.")
	flag.StringVar(&filterPath, "filter_path", viper.GetString("filter_path"), "Only convert files uploaded to specific path. Advanced, leave default for immich")
	flag.StringVar(&filterFormKey, "filter_form_key", viper.GetString("filter_form_key"), "Only convert files uploaded with specific form key. Advanced, leave default for immich")
	flag.Parse()
	validateInput()
}

func validateInput() {
	if upstreamURL == "" {
		log.Fatal("the -upstream flag is required")
	}

	var err error
	remote, err = url.Parse(upstreamURL)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	convertCMDTemplate, err = template.New("command").Parse(convertCMD)
	if err != nil {
		log.Fatalf("invalid convert command: %v", err)
	}

	values := map[string]string{
		"folder":    "/test",
		"name":      "file",
		"extension": "ext",
	}
	var cmdLine bytes.Buffer
	err = convertCMDTemplate.Execute(&cmdLine, values)
	if err != nil {
		log.Fatalf("invalid convert command: %v", err)
	}
}

func processFile(file io.Reader, originalExtension string) (processedFile io.Reader, newExtension string, newSize int64, err error) {
	tempDir, err := os.MkdirTemp("", "processing-*")
	if err != nil {
		err = fmt.Errorf("unable to create temp folder: %w", err)
		return
	}
	defer os.RemoveAll(tempDir)

	tempFile, err := os.CreateTemp(tempDir, "input-*"+originalExtension)
	if err != nil {
		err = fmt.Errorf("unable to create temp file: %w", err)
		return
	}

	_, err = io.Copy(tempFile, file)
	if err != nil {
		err = fmt.Errorf("unable to write temp file: %w", err)
		return
	}
	tempFile.Close()

	basename := path.Base(tempFile.Name())
	extension := path.Ext(basename)
	values := map[string]string{
		"folder":    tempDir,
		"name":      strings.TrimSuffix(basename, extension),
		"extension": strings.TrimPrefix(extension, "."),
	}
	var cmdLine bytes.Buffer
	err = convertCMDTemplate.Execute(&cmdLine, values)
	if err != nil {
		err = fmt.Errorf("unable to generate convert command: %w", err)
		return
	}

	cmd := exec.Command("sh", "-c", cmdLine.String())
	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("unable to run convert command \"%s\", error: %w", cmdLine.String(), err)
		return
	}

	files, err := os.ReadDir(tempDir)
	if err != nil {
		err = fmt.Errorf("unable to read temp directory: %w", err)
		return
	}

	if len(files) != 1 {
		err = fmt.Errorf("unexpected number of files in temp directory: %d", len(files))
		return
	}

	processedFile, err = os.Open(path.Join(tempDir, files[0].Name()))
	if err != nil {
		err = fmt.Errorf("unable to open temp file: %w", err)
		return
	}

	newExtension = path.Ext(files[0].Name())

	stat, err := os.Stat(path.Join(tempDir, files[0].Name()))
	if err != nil {
		err = fmt.Errorf("unable to get file size: %w", err)
	}
	newSize = stat.Size()

	return
}

func handleMultipartUpload(w http.ResponseWriter, r *http.Request, formFileKey string) (originalFilename string, originalSize int64, newFilename string, newSize int64, replaced bool, err error) {
	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	replaced = false
	newFilename = "FILE-NOT-PROCESSED"

	err = r.ParseMultipartForm(100 << 30) // 100 MB max memory
	if err != nil {
		err = fmt.Errorf("unable to parse multipart form: %w", err)
		return
	}

	originalFile, handler, err := r.FormFile(formFileKey)
	if err != nil {
		err = fmt.Errorf("unable to read form file key %s in uploaded form data: %w", formFileKey, err)
		return
	}
	defer originalFile.Close()

	originalSize = handler.Size
	originalFilename = handler.Filename
	basename := path.Base(originalFilename)
	extension := path.Ext(basename)

	shouldProcess := false
	if len(extensionWhitelist) < 1 {
		shouldProcess = true
	} else {
		whitelist := strings.Split(extensionWhitelist, ",")

		extCheck := strings.TrimPrefix(extension, ".")
		for _, ext := range whitelist {
			if strings.EqualFold(ext, extCheck) {
				shouldProcess = true
				break
			}
		}
	}

	var processedFile io.Reader
	var newExtension string
	if shouldProcess {
		processedFile, newExtension, newSize, err = processFile(originalFile, extension)
		if err != nil {
			err = fmt.Errorf("unable to process file: %w", err)
			return
		}

		newFilename = strings.TrimSuffix(originalFilename, extension) + newExtension

		replaced = originalSize > newSize
	} else {
		newFilename = "EXTENSION-NOT-IN-WHITELIST"
	}

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	for key, values := range r.MultipartForm.Value {
		for _, value := range values {
			err = writer.WriteField(key, value)
			if err != nil {
				err = fmt.Errorf("unable to create form data to be sent upstream: %w", err)
				return
			}
		}
	}

	uploadFilename := originalFilename
	if replaced {
		uploadFilename = newFilename
	}
	part, err := writer.CreateFormFile(formFileKey, uploadFilename)
	if err != nil {
		err = fmt.Errorf("unable to create file form field to be sent upstream: %w", err)
		return
	}

	if replaced {
		_, err = io.Copy(part, processedFile)
	} else {
		_, err = io.Copy(part, originalFile)
	}
	if err != nil {
		err = fmt.Errorf("unable to write file in form field to be sent upstream: %w", err)
		return
	}

	err = writer.Close()
	if err != nil {
		err = fmt.Errorf("unable to finish form data to be sent upstream: %w", err)
		return
	}

	destination := *remote
	destination.Path = path.Join(destination.Path, r.URL.Path)
	req, err := http.NewRequest("POST", destination.String(), &buffer)
	if err != nil {
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
		err = fmt.Errorf("unable to POST to upstream: %w", err)
		return
	}
	defer resp.Body.Close()

	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		err = fmt.Errorf("unable forward response back to client from upstream: %w", err)
		return
	}

	return
}

func main() {
	proxy := httputil.NewSingleHostReverseProxy(remote)

	handler := func(w http.ResponseWriter, r *http.Request) {
		match, err := path.Match(filterPath, r.URL.Path)
		if err != nil {
			log.Printf("invalid filter_path: %s", r.URL)
			return
		}
		if !match || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			log.Printf("proxy request: %s", r.URL)

			r.Host = remote.Host
			proxy.ServeHTTP(w, r)

			return
		}

		currentJobID++
		jobID := currentJobID
		log.Printf("job %d: incoming file upload on \"%s\" from %s, intercepting...", jobID, r.URL, r.RemoteAddr)
		originalFilename, originalSize, newFilename, newSize, replaced, err := handleMultipartUpload(w, r, filterFormKey)
		if err != nil {
			log.Printf("job %d: Failed to process upload: %v", jobID, err.Error())
			http.Error(w, "failed to process upload, view logs for more info", http.StatusInternalServerError)
			return
		}

		action := "file NOT replaced"
		if replaced {
			action = "file replaced"
		}

		log.Printf("job %d: %s: \"%s\" %s optimized to \"%s\" %s", jobID, action, originalFilename, humanReadableSize(originalSize), newFilename, humanReadableSize(newSize))
	}

	http.HandleFunc("/", handler)

	log.Printf("Starting immich-upload-optimizer on %s...", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("Error starting immich-upload-optimizer: %v", err)
	}
}

func humanReadableSize(size int64) string {
	const (
		_  = iota // ignore first value by assigning to blank identifier
		KB = 1 << (10 * iota)
		MB
		GB
		TB
	)

	switch {
	case size >= TB:
		return fmt.Sprintf("%.2f TB", float64(size)/TB)
	case size >= GB:
		return fmt.Sprintf("%.2f GB", float64(size)/GB)
	case size >= MB:
		return fmt.Sprintf("%.2f MB", float64(size)/MB)
	case size >= KB:
		return fmt.Sprintf("%.2f KB", float64(size)/KB)
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}
