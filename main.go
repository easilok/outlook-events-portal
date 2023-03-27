package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"net/http"

	"github.com/BurntSushi/toml"
	"github.com/easilok/outlook_event_reading/authentication"
	"github.com/sirupsen/logrus"
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
	Lock       sync.Mutex
	LastUpdate time.Time
	Context    string         `json:"@odata.context"`
	Value      []OutlookEvent `json:"value"`
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
var graphCredentials *authentication.CredentialStorage

var log *logrus.Logger

func loadConfig() {
	configContent, err := os.ReadFile("config.toml") // the file is inside the local directory
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

func main() {
	loadConfig()
	fmt.Printf("%+v\n", applicationConfig)
	log = logrus.New()
	log.SetLevel(logrus.DebugLevel)
	log.SetFormatter(&logrus.TextFormatter{
		// DisableColors: true,
		FullTimestamp: true,
	})
	// If you wish to add the calling method as a field, instruct the logger via:
	// log.SetReportCaller(true)

	// Start a server on port 8000
	servingHost := fmt.Sprintf("%s:%d", serverHost, serverPort)
	log.Info(fmt.Sprintf("Serving login server on %s\n", applicationConfig.OauthConfig.BaseURL()))
	l, err := net.Listen("tcp", servingHost)
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/next-event", nextEventHandler)

	graphCredentials = authentication.Run(&applicationConfig.OauthConfig, &applicationConfig.CredentialsConfig, log)
	go outlookEventRefreshManager()
	log.Fatal(http.Serve(l, nil))
}

func nextEventHandler(w http.ResponseWriter, r *http.Request) {
	if len(graphEvents.Value) > 0 {
		for _, nextEvent := range graphEvents.Value {
			nextEventDateTime, _ := time.Parse("2006-01-02T15:04:05", nextEvent.Start.DateTime)
			nextEventDateTime = nextEventDateTime.In(time.Local)
			if nextEventDateTime.After(time.Now()) {
				fmt.Fprintf(w, "%s - %d:%d (%d/%d) %s", nextEvent.Subject, nextEventDateTime.Hour(), nextEventDateTime.Minute(), nextEventDateTime.Day(), nextEventDateTime.Month(), nextEventDateTime.Location())
				return
			}
		}
	} else {
		fmt.Fprint(w, "No events")
	}
}

func outlookEventRefreshManager() {
	// This manager is a infinite loop only stopping if token refresh results in error
	for {
		fetchEventsTicker := 60 * time.Second

		time.Sleep(fetchEventsTicker)

		log.Debug(fmt.Sprintf("***Events Manager: currently running goroutines: %d***", runtime.NumGoroutine()))

		fetchEventsFromOutlook()
	}
}

func fetchEventsFromOutlook() {
	// Get accessToken and authentication status
	graphAccessToken, isAuthenticated := graphCredentials.GetAccessToken()

	if !isAuthenticated {
		log.Warn("Application is not yet authenticated")
		return
	}

	// Get the current date and the date for tomorrow
	now := time.Now().UTC()
	tomorrow := now.Add(time.Hour * 72).UTC()

	// Format the dates for the API request
	startDate := now.Format("2006-01-02T15:04:05")
	endDate := tomorrow.Format("2006-01-02T15:04:05")

	// eventsEndpoint := "https://graph.microsoft.com/v1.0/me/events?$select=subject,body,start,end,location"
	calendarViewEndpoint := fmt.Sprintf("https://graph.microsoft.com/v1.0/me/calendarview?startdatetime=%s&enddatetime=%s&$select=subject,start,end,location", startDate, endDate)

	eventsReq, err := http.NewRequest("GET", calendarViewEndpoint, nil)
	if err != nil {
		log.Error(fmt.Sprintf("Error creating event list request: %s", err.Error()))
		return
	}
	eventsReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", graphAccessToken))
	client := &http.Client{}
	eventsResponse, err := client.Do(eventsReq)
	if err != nil {
		log.Error(fmt.Sprintf("Error acquiring event list: %s", err.Error()))
		return
	}

	// var graphEvents map[string]interface{}
	err = json.NewDecoder(eventsResponse.Body).Decode(&graphEvents)
	eventsResponse.Body.Close()
	if err != nil {
		log.Error(fmt.Sprintf("Error decoding outlook events: %s", err))
		return
	}

	graphEvents.LastUpdate = time.Now()

	// Print the details of each meeting
	log.Info(fmt.Sprintf("Fetched %d events from outlook", len(graphEvents.Value)))

	sort.Slice(graphEvents.Value, func(i, j int) bool {
		return graphEvents.Value[i].Start.DateTime < graphEvents.Value[j].Start.DateTime
	})

	for i, event := range graphEvents.Value {
		log.Info(fmt.Sprintf("Event %d; Subject: %s; Start time: %s; End time: %s;", i+1, event.Subject, event.Start.DateTime, event.End.DateTime))
	}

	graphEvents.persistEvent("/tmp")

}

func (outlookEventList *OutlookEventList) persistEvent(path string) {
	if len(path) == 0 {
		return
	}

	outlookEventList.Lock.Lock()
	defer outlookEventList.Lock.Unlock()

	nextEventString := "No events"

	if len(outlookEventList.Value) > 0 {
		for _, nextEvent := range outlookEventList.Value {
			nextEventDateTime, _ := time.Parse("2006-01-02T15:04:05", nextEvent.Start.DateTime)
			nextEventEndDateTime, _ := time.Parse("2006-01-02T15:04:05", nextEvent.End.DateTime)
			nextEventDateTime = nextEventDateTime.In(time.Local)
			nextEventEndDateTime = nextEventEndDateTime.In(time.Local)
			if nextEventDateTime.After(time.Now()) {
				// nextEventString = fmt.Sprintf("%s - %d:%d (%d/%d)", nextEvent.Subject, nextEventDateTime.Hour(), nextEventDateTime.Minute(), nextEventDateTime.Day(), nextEventDateTime.Month())
				nextEventString = fmt.Sprintf("%s - %s - %s", nextEvent.Subject, nextEventDateTime.Format("15:04 (02/Jan)"), nextEventEndDateTime.Format("15:04 (02/Jan)"))
				break
			}
		}
	}

	// create the file
	f, err := os.Create(filepath.Join(path, "next_event"))
	if err != nil {
		log.Error(fmt.Sprintf("Error creating next event file: %s", err.Error()))
		return
	}

	// write a string
	_, err = f.WriteString(nextEventString)
	if err != nil {
		log.Error(fmt.Sprintf("Error writing next event file: %s", err.Error()))
	}
	// close the file with defer
	err = f.Close()
	if err != nil {
		log.Error(fmt.Sprintf("Error closing next event file: %s", err.Error()))
	}
}
