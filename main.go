package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/spf13/viper"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var remote *url.URL

var (
	maxConcurrentRequests = 10
	semaphore             = make(chan struct{}, maxConcurrentRequests)
)

var jobsMu sync.Mutex
var jobChannels = make(map[string]chan *http.Response)
var jobChannelsComplete = make(map[string]chan struct{})

var showVersion bool
var upstreamURL string
var listenAddr string
var configFile string
var filterPath string
var filterFormKey string

var config *Config

func init() {
	viper.SetEnvPrefix("iuo")
	viper.AutomaticEnv()
	viper.BindEnv("upstream")
	viper.BindEnv("listen")
	viper.BindEnv("tasks_file")
	viper.BindEnv("filter_path")
	viper.BindEnv("filter_form_key")

	viper.SetDefault("upstream", "")
	viper.SetDefault("listen", ":2283")
	viper.SetDefault("tasks_file", "tasks.yaml")
	viper.SetDefault("filter_path", "/api/assets")
	viper.SetDefault("filter_form_key", "assetData")

	flag.BoolVar(&showVersion, "version", false, "Show the current version")
	flag.StringVar(&upstreamURL, "upstream", viper.GetString("upstream"), "Upstream URL. Example: http://immich-server:2283")
	flag.StringVar(&listenAddr, "listen", viper.GetString("listen"), "Listening address")
	flag.StringVar(&configFile, "tasks_file", viper.GetString("tasks_file"), "Path to the configuration file")
	flag.StringVar(&filterPath, "filter_path", viper.GetString("filter_path"), "Only convert files uploaded to specific path. Advanced, leave default for immich")
	flag.StringVar(&filterFormKey, "filter_form_key", viper.GetString("filter_form_key"), "Only convert files uploaded with specific form key. Advanced, leave default for immich")
	flag.Parse()

	if showVersion {
		fmt.Println(printVersion())
		os.Exit(0)
	}

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

	if configFile == "" {
		log.Fatal("the -tasks_file flag is required")
	}

	config, err = NewConfig(&configFile)
	if err != nil {
		log.Fatalf("error loading config file: %v", err)
	}
}

func main() {
	baseLogger := log.New(os.Stdout, "", log.Ldate|log.Ltime)

	proxy := httputil.NewSingleHostReverseProxy(remote)

	handler := func(w http.ResponseWriter, r *http.Request) {
		requestLogger := newCustomLogger(baseLogger, fmt.Sprintf("%s: ", strings.Split(r.RemoteAddr, ":")[0]))

		if r.URL.Path == "/_immich-upload-optimizer/wait" {
			continueJob(r, w, requestLogger)
			return
		}

		match, err := path.Match(filterPath, r.URL.Path)
		if err != nil {
			requestLogger.Printf("invalid filter_path: %s", r.URL)
			return
		}
		if match && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			err = newJob(r, w, requestLogger)
			if err != nil {
				requestLogger.Printf("upload handler error: %v", err)
			}
			return
		}

		requestLogger.Printf("proxy: %s", r.URL)

		r.Host = remote.Host
		proxy.ServeHTTP(w, r)
	}

	server := &http.Server{
		Addr:    listenAddr,
		Handler: http.HandlerFunc(handler),
	}

	log.Printf("Starting %s on %s...", printVersion(), listenAddr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Error starting immich-upload-optimizer: %v", err)
	}
}

func printVersion() string {
	return fmt.Sprintf("immich-upload-optimizer %s, commit %s, built at %s", version, commit, date)
}
