package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"path/filepath"
	"sync"
	"time"

	"log"
	"net/http"

	"github.com/BurntSushi/toml"
	"github.com/easilok/outlook_event_reading/authentication"
)

type GraphAuthentication struct {
	Token        string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    uint16 `json:"expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type GraphApiLocation struct {
	Name         string `json:"displayName"`
	LocationType string `json:"locationType"`
	Id           string `json:"uniqueId"`
	IdType       string `json:"uniqueIdType"`
}

type GraphApiDateTime struct {
	DateTime string `json:"dateTime"`
	Timezone string `json:"timeZone"`
}

type OutlookEvent struct {
	Id       string           `json:"id"`
	Subject  string           `json:"subject"`
	Location GraphApiLocation `json:"location"`
	Start    GraphApiDateTime `json:"start"`
	End      GraphApiDateTime `json:"end"`
}

type OutlookEventList struct {
	Lock    sync.Mutex
	Context string         `json:"@odata.context"`
	Value   []OutlookEvent `json:"value"`
}

type ApplicationConfig struct {
	OauthConfig       authentication.OauthConfig
	CredentialsConfig authentication.CredentialsConfig
}

const (
	defaultProtocol = "http"
	defaultHost     = "localhost"
	defaultPort     = 8000
)

var clientID = ""
var clientSecret = ""
var tenantID = ""
var serverProtocol = defaultProtocol
var serverHost = defaultHost
var serverPort = defaultPort
var redirectURI = fmt.Sprintf("%s://%s:%d/callback", serverProtocol, serverHost, serverPort)

var graphEvents OutlookEventList
var applicationConfig ApplicationConfig
var graphCredentials authentication.CredentialStorage

func loadConfig() {
	configContent, err := ioutil.ReadFile("config.toml") // the file is inside the local directory
	if err != nil {
		log.Fatal(err.Error())
	}

	_, err = toml.Decode(string(configContent), &applicationConfig)
	if err != nil {
		log.Fatal(err.Error())
	}

	clientID = applicationConfig.OauthConfig.ClientID
	clientSecret = applicationConfig.OauthConfig.ClientSecret
	tenantID = applicationConfig.OauthConfig.TenantID

	if len(applicationConfig.OauthConfig.ServerProtocol) > 0 {
		serverProtocol = applicationConfig.OauthConfig.ServerProtocol
	}
	if len(applicationConfig.OauthConfig.ServerHost) > 0 {
		serverHost = applicationConfig.OauthConfig.ServerHost
	}
	if applicationConfig.OauthConfig.ServerPort > 0 {
		serverPort = applicationConfig.OauthConfig.ServerPort
	}
}

func loadCredentials() {
	// Initialize variable
	graphCredentials = authentication.CredentialStorage{}
	// Only load if persistent storage is enabled
	if !applicationConfig.CredentialsConfig.Persist {
		return
	}
	credentialsContent, err := ioutil.ReadFile(filepath.Join(applicationConfig.CredentialsConfig.StoragePath, "credentials.toml"))
	if err != nil {
		fmt.Println("Error loading existing credentials file: ", err.Error())
	}

	_, err = toml.Decode(string(credentialsContent), &graphCredentials.Credentials)
	if err != nil {
		fmt.Println("Error decoding existing credentials file: ", err.Error())
	}
}

func main() {
	loadConfig()
	fmt.Println(applicationConfig)
	loadCredentials()
	// Start a server on port 8000
	servingHost := fmt.Sprintf("%s:%d", serverHost, serverPort)
	fmt.Printf("Serving login server on %s\n", applicationConfig.OauthConfig.BaseURL())
	l, err := net.Listen("tcp", servingHost)
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/next-event", nextEventHandler)

	graphCredentials.Run(&applicationConfig.OauthConfig, &applicationConfig.CredentialsConfig)
	// log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", serverPort), nil))
	go outlookEventRefreshManager()
	log.Fatal(http.Serve(l, nil))
}

func nextEventHandler(w http.ResponseWriter, r *http.Request) {
	if len(graphEvents.Value) > 0 {
		nextEvent := graphEvents.Value[0]
		nextEventDateTime, _ := time.Parse("2006-01-02T15:04:05", nextEvent.Start.DateTime)
		fmt.Fprintf(w, "%s - %d:%d", nextEvent.Subject, nextEventDateTime.Hour(), nextEventDateTime.Minute())
	} else {
		fmt.Fprint(w, "No events")
	}
}

func outlookEventRefreshManager() {
	// This manager is a infinite loop only stopping if token refresh results in error
	for {
		fetchEventsTicker := 10 * time.Second

		<-time.After(fetchEventsTicker)

		fetchEventsFromOutlook()
	}
}

func fetchEventsFromOutlook() {
	// Use object lock for multithread access
	graphCredentials.Lock.Lock()
	isAuthenticated := graphCredentials.Authenticated
	// Save access token to release lock from now on
	graphAccessToken := graphCredentials.Credentials.AccessToken
	graphCredentials.Lock.Unlock()

	if !isAuthenticated {
		fmt.Println("Application is not yet authenticated")
		return
	}

	// Get the current date and the date for tomorrow
	now := time.Now()
	tomorrow := now.Add(time.Hour * 72)

	// Format the dates for the API request
	startDate := now.Format("2006-01-02T15:04:05")
	endDate := tomorrow.Format("2006-01-02T15:04:05")

	// eventsEndpoint := "https://graph.microsoft.com/v1.0/me/events?$select=subject,body,start,end,location"
	calendarViewEndpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/me/calendarview?startdatetime=%s&enddatetime=%s&$select=subject,start,end,location", startDate, endDate)

	eventsReq, err := http.NewRequest("GET", calendarViewEndpoint, nil)
	if err != nil {
		fmt.Println("Error acquiring token:", err)
		return
	}
	eventsReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", graphAccessToken))
	client := &http.Client{}
	eventsResponse, err := client.Do(eventsReq)
	if err != nil {
		fmt.Println("Error acquiring token:", err)
		return
	}

	// var graphEvents map[string]interface{}
	json.NewDecoder(eventsResponse.Body).Decode(&graphEvents)
	// fmt.Println("Events response: ", graphEvents)

	// Print the details of each meeting
	fmt.Printf("Fetched %d events from outlook\n", len(graphEvents.Value))

	for i, event := range graphEvents.Value {
		fmt.Printf("Event %d:\n", i+1)
		fmt.Printf("Subject: %s\n", event.Subject)
		fmt.Printf("Start time: %s\n", event.Start.DateTime)
		fmt.Printf("End time: %s\n\n", event.End.DateTime)
	}
}
