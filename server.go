package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/kennygrant/sanitize"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"strconv"
)

type eventPayload interface{}

type mailjetAPIEvent struct {
	Event string `json:"event"`
}

type eventItem struct {
	EventType string
	Payload   eventPayload
}

type messagePayload struct {
	FromEmail string
	Recipient string
	Body      string
	Subject   string
}

type mailjetAPIMessagePayload struct {
	FromEmail string
	Subject   string
	To string
	Body      string `json:"Html-part"`
}

type eventSetupPayload struct {
	EventType   string
	CallbackUrl string
}

type mailjetAPIEventCallbackUrlPayload struct {
	EventType string
	Url       string
}

type mailjetConfig struct {
	BaseUrl        string            `json:"base_url"`
	MaxEventsCount int               `json:"max_events_count"`
	Default        map[string]string `json:"default"`
}

type apiError struct {
	ErrorMessage string
}

const defaultAddr = "127.0.0.1"
const defaultPort = 3000
const dataFileBaseName = "events_%s.json"
const defaultConfigFilePath = "./config.json"
const eventCallbackUrlBaseUrl = "/v3/REST/eventcallbackurl"

var eventMutex = new(sync.Mutex)

var config = mailjetConfig{}

var TraceLogger *log.Logger
var ErrorLogger *log.Logger

func handleError(w http.ResponseWriter, message string, status int) {
	ErrorLogger.Println(status, message)

	e := apiError{ErrorMessage: message}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(e)
}

func handleAuth(r *http.Request) (string, string, error) {
	username, password, ok := r.BasicAuth()
	var err string
	if !ok {
		err = "Error when reading auth"
	}

	if username == "" {
		err = "API key is mandatory"
	}

	if password == "" {
		err = "API secret is mandatory"
	}

	if err != "" {
		return username, password, errors.New(err)
	}

	return username, password, nil
}

// Handle events
func handleEvents(w http.ResponseWriter, r *http.Request) {
	// Since multiple requests could come in at once, ensure we have a lock
	// around all file operations
	eventMutex.Lock()
	defer eventMutex.Unlock()

	vars := mux.Vars(r)
	apiKey := sanitize.BaseName(vars["apikey"])
	if apiKey == "" {
		handleError(w, "An API Key must be provided", http.StatusBadRequest)
		return
	}

	dataFileSession := fmt.Sprintf(dataFileBaseName, apiKey)

	// Stat the file, so we can find its current permissions
	var fi os.FileInfo
	var errStat error
	fi, errStat = os.Stat(dataFileSession)
	if errStat != nil {
		f, err := os.Create(dataFileSession)
		if err != nil {
			handleError(w, "Error when creating session file", http.StatusInternalServerError)
		}

		fi, _ = f.Stat()
		f.WriteString("[]")
		f.Close()
	}

	// Read the events from the file.
	eventData, err := ioutil.ReadFile(dataFileSession)
	if err != nil {
		handleError(w, fmt.Sprintf("Unable to read the data file (%s): %s", dataFileSession, err), http.StatusInternalServerError)
		return
	}

	switch r.Method {
	case "POST":
		// Decode the JSON data
		events := make([]eventItem, 0)
		if err := json.Unmarshal(eventData, &events); err != nil {
			handleError(w, fmt.Sprintf("Unable to Unmarshal events from data file (%s): %s", dataFileSession, err), http.StatusInternalServerError)
			return
		}

		response, _ := ioutil.ReadAll(r.Body)
		TraceLogger.Println("New event payload received", string(response))

		// Add a new event to the in memory slice of events
		var mjEvent mailjetAPIEvent
		err1 := json.Unmarshal(response, &mjEvent)
		if err1 != nil {
			handleError(w, err1.Error(), http.StatusBadRequest)
			return
		}

		var mjEventPayload eventPayload
		json.Unmarshal(response, &mjEventPayload)
		newEventItem := eventItem{
			EventType: mjEvent.Event,
			Payload:   mjEventPayload,
		}
		events = append([]eventItem{newEventItem}, events...)
		if config.MaxEventsCount > 0 && len(events) > config.MaxEventsCount {
			events = events[:config.MaxEventsCount]
		}

		// Marshal the events to indented json.
		var err3 error
		eventData, err3 = json.Marshal(events)
		if err3 != nil {
			handleError(w, fmt.Sprintf("Unable to marshal events to json: %s", err3), http.StatusInternalServerError)
			return
		}

		// Write out the events to the file, preserving permissions
		err2 := ioutil.WriteFile(dataFileSession, eventData, fi.Mode())
		if err2 != nil {
			handleError(w, fmt.Sprintf("Unable to write events to data file (%s): %s", dataFileSession, err3), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		io.Copy(w, bytes.NewReader(eventData))

	case "GET":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		// stream the contents of the file to the response
		io.Copy(w, bytes.NewReader(eventData))

	default:
		// Don't know the method, so error
		handleError(w, fmt.Sprintf("Unsupported method: %s", r.Method), http.StatusMethodNotAllowed)
	}
}

// Handle messages
func handleMessages(w http.ResponseWriter, r *http.Request) {

	switch r.Method {
	case "POST":
		reqBody, _ := ioutil.ReadAll(r.Body)
		TraceLogger.Println("New message payload received", string(reqBody))

		messagePayload := messagePayload{}
		err := json.Unmarshal(reqBody, &messagePayload)
		if err != nil {
			handleError(w, err.Error(), http.StatusBadRequest)
			return
		}

		username, password, authError := handleAuth(r)
		if authError != nil {
			handleError(w, authError.Error(), http.StatusUnauthorized)
			return
		}

		if messagePayload.FromEmail == "" {
			handleError(w, "FromEmail is mandatory", http.StatusBadRequest)
			return
		}

		if messagePayload.Recipient == "" {
			messagePayload.Recipient = messagePayload.FromEmail
		}

		if messagePayload.Subject == "" {
			handleError(w, "Subject is mandatory", http.StatusBadRequest)
			return
		}

		if messagePayload.Body == "" {
			handleError(w, "Body is mandatory", http.StatusBadRequest)
			return
		}

		payload := mailjetAPIMessagePayload{
			FromEmail: messagePayload.FromEmail,
			To: messagePayload.Recipient,
			Subject:   messagePayload.Subject,
			Body:      messagePayload.Body,
		}
		payloadMarshalled, err := json.Marshal(payload)
		if err != nil {
			handleError(w, fmt.Sprintf("Error when marshalling payload : %s", err), http.StatusInternalServerError)
			return
		}

		client := &http.Client{}
		req, _ := http.NewRequest("POST", config.BaseUrl+"/v3/send/message", bytes.NewReader(payloadMarshalled))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(username, password)

		mailjetResponse, err := client.Do(req)
		if mailjetResponse.StatusCode != 200 {
			handleError(w, mailjetResponse.Status, mailjetResponse.StatusCode)
			return
		}
		if err != nil {
			handleError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		TraceLogger.Println("Payload POST-ed to Mailjet Send API", mailjetResponse)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		io.Copy(w, bytes.NewReader(reqBody))

	default:
		// Don't know the method, so error
		handleError(w, fmt.Sprintf("Unsupported method: %s", r.Method), http.StatusMethodNotAllowed)
	}
}

// Handle messages
func handleEventSetup(w http.ResponseWriter, r *http.Request) {

	switch r.Method {
	case "POST":
		reqMessage, _ := ioutil.ReadAll(r.Body)
		TraceLogger.Println("New event setup payload received", string(reqMessage))

		p := eventSetupPayload{}
		err := json.Unmarshal(reqMessage, &p)
		if err != nil {
			handleError(w, err.Error(), http.StatusBadRequest)
			return
		}

		username, password, authError := handleAuth(r)
		if authError != nil {
			handleError(w, authError.Error(), http.StatusUnauthorized)
			return
		}

		if p.EventType == "" {
			handleError(w, "EventType is mandatory", http.StatusBadRequest)
			return
		}

		if p.CallbackUrl == "" {
			handleError(w, "CallbackUrl is mandatory", http.StatusBadRequest)
			return
		}

		mjPayload := mailjetAPIEventCallbackUrlPayload{
			EventType: p.EventType,
			Url:       p.CallbackUrl,
		}
		payloadMarshalled, err := json.Marshal(mjPayload)
		if err != nil {
			handleError(w, fmt.Sprintf("Error when marshalling payload : %s", err), http.StatusInternalServerError)
			return
		}

		client := &http.Client{}

		baseEventUrl, _ := url.Parse(fmt.Sprintf("%s/%s", config.BaseUrl, eventCallbackUrlBaseUrl))
		eventUrl, err := url.Parse(fmt.Sprintf("%s/%s", baseEventUrl, fmt.Sprintf("%s|%t", p.EventType, false)))
		if err != nil {
			TraceLogger.Println("Error while building event url", err)
			handleError(w, fmt.Sprintf("Error while building event url : %s", err), http.StatusInternalServerError)
			return
		}

		getReq, _ := http.NewRequest("GET", eventUrl.String(), nil)
		getReq.SetBasicAuth(username, password)
		getResponse, err := client.Do(getReq)
		if err != nil {
			handleError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		TraceLogger.Println("Mailjet API GET response to", eventUrl.String(), getResponse)
		if getResponse.StatusCode != 200 && getResponse.StatusCode != 404 {
			handleError(w, getResponse.Status, getResponse.StatusCode)
			return
		} else if getResponse.StatusCode == 404 {
			postReq, _ := http.NewRequest("POST", baseEventUrl.String(), bytes.NewReader(payloadMarshalled))
			postReq.Header.Set("Content-Type", "application/json")
			postReq.SetBasicAuth(username, password)
			postResponse, err := client.Do(postReq)

			TraceLogger.Println("Mailjet API POST response to", baseEventUrl.String(), postResponse)
			if err != nil {
				handleError(w, err.Error(), postResponse.StatusCode)
				return
			}
			if postResponse.StatusCode != 201 && postResponse.StatusCode != 200 {
				handleError(w, postResponse.Status, postResponse.StatusCode)
				return
			}
		} else if getResponse.StatusCode == 200 {
			putReq, _ := http.NewRequest("PUT", eventUrl.String(), bytes.NewReader(payloadMarshalled))
			putReq.SetBasicAuth(username, password)
			putReq.Header.Set("Content-Type", "application/json")
			putResponse, err := client.Do(putReq)

			TraceLogger.Println("Mailjet API PUT response to", eventUrl, putResponse)
			if err != nil {
				handleError(w, err.Error(), putResponse.StatusCode)
				return
			}
			if putResponse.StatusCode != 200 {
				handleError(w, putResponse.Status, putResponse.StatusCode)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, bytes.NewReader(reqMessage))

	default:
		// Don't know the method, so error
		handleError(w, fmt.Sprintf("Unsupported method: %s", r.Method), http.StatusMethodNotAllowed)
	}
}

// Handle messages
func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		configJson, _ := json.Marshal(&config)

		w.Header().Set("Content-Type", "application/json")
		io.Copy(w, bytes.NewReader(configJson))
	default:
		// Don't know the method, so error
		handleError(w, fmt.Sprintf("Unsupported method: %s", r.Method), http.StatusMethodNotAllowed)
	}
}

func main() {
	port := -1
	addr := ""
	configFilePath := ""

	if len(os.Args) >= 2 {
		portParam, err := strconv.Atoi(os.Args[1])
		if err != nil {
			log.Fatal(fmt.Sprintf("Unable to read the port from command line: %s: %s", portParam, err), http.StatusInternalServerError)
		}
		port = portParam
	}

	if len(os.Args) >= 3 {
		addr = os.Args[2]
	}

	if len(os.Args) >= 4 {
		configFilePath = os.Args[3]
	}

	if port == -1 {
		port = defaultPort
	}

	if addr == "" {
		addr = defaultAddr
	}

	if configFilePath == "" {
		configFilePath = defaultConfigFilePath
	}

	TraceLogger = log.New(os.Stdout,
		"TRACE: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	ErrorLogger = log.New(os.Stderr,
		"ERROR: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	// Read the events from the file.
	configFile, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		log.Fatal(fmt.Sprintf("Unable to read the config file (%s): %s", configFilePath, err), http.StatusInternalServerError)
		return
	}
	json.Unmarshal(configFile, &config)
	TraceLogger.Println(fmt.Sprintf("Read config %s: %+v", configFilePath, config))

	r := mux.NewRouter()
	r.HandleFunc("/config", handleConfig)
	r.HandleFunc("/apikey/{apikey}/events", handleEvents)
	r.HandleFunc("/apikey/{apikey}/events/setup", handleEventSetup)
	r.HandleFunc("/messages", handleMessages)

	r.PathPrefix("/").Handler(http.FileServer(http.Dir("./public")))
	http.Handle("/", r)

	TraceLogger.Println(fmt.Sprintf("Server started: http://%s:%d", addr, port))
	log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", addr, port), nil))
}
