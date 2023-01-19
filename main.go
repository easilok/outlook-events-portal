package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os/exec"
	"time"

	"log"
	"net/http"
	"net/url"

	"github.com/BurntSushi/toml"
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
	Context string         `json:"@odata.context"`
	Value   []OutlookEvent `json:"value"`
}

type OauthConfig struct {
	ClientID       string
	ClientSecret   string
	TenantID       string
	ServerProtocol string
	ServerHost     string
	ServerPort     int
}

type ApplicationConfig struct {
	OauthConfig OauthConfig
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

func main() {
	loadConfig()
	fmt.Println(applicationConfig)
	// Start a server on port 8000
	http.HandleFunc("/", home)
	http.HandleFunc("/login", login)
	http.HandleFunc("/success", success)
	http.HandleFunc("/callback", callback)
	http.HandleFunc("/next-event", nextEventHandler)
	servingHost := fmt.Sprintf("%s:%d", serverHost, serverPort)
	fmt.Printf("Serving login server on %s://%s\n", serverProtocol, servingHost)
	l, err := net.Listen("tcp", servingHost)
	if err != nil {
		log.Fatal(err)
	}
	// The browser can connect now because the listening socket is open.
	browserOpenErr := exec.Command("open", fmt.Sprintf("%s://%s", serverProtocol, servingHost)).Start()
	if browserOpenErr != nil {
		fmt.Printf("Error opening browser to login: %s\n", browserOpenErr.Error())
	}

	// log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", serverPort), nil))
	log.Fatal(http.Serve(l, nil))
}

func home(w http.ResponseWriter, r *http.Request) {
	// Display a link to the login page
	fmt.Fprint(w, "<a href='/login'>Login</a>")
}

func success(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "<p>Login success. You can not close this tab.</p>")
}

func login(w http.ResponseWriter, r *http.Request) {
	// Redirect the user to the Microsoft login page
	url := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/authorize?client_id=%s&response_type=code&redirect_uri=%s&scope=Calendars.Read", tenantID, clientID, redirectURI)
	http.Redirect(w, r, url, http.StatusFound)
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

func callback(w http.ResponseWriter, r *http.Request) {
	// Get the authorization code from the query parameter
	code := r.URL.Query().Get("code")

	fmt.Println("fetched code: ", code)

	// Use the authorization code to acquire an access token
	data := url.Values{
		"client_id":     {clientID},
		"scope":         {"https://graph.microsoft.com/.default offline_access"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"client_secret": {clientSecret},
	}

	resp, err := http.PostForm(fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID), data)
	if err != nil {
		fmt.Println("Error acquiring token:", err)
		return
	}

	// var graphAuth map[string]interface{}
	var graphAuth GraphAuthentication

	fmt.Println("Req code: ", resp.StatusCode)
	fmt.Println("Resp body: ", resp.Body)
	json.NewDecoder(resp.Body).Decode(&graphAuth)

	fmt.Println("Token type: ", graphAuth.Token)
	fmt.Println("Access Token: ", graphAuth.AccessToken)
	fmt.Println("Token expires in (s): ", graphAuth.ExpiresIn)
	fmt.Println("Token sope: ", graphAuth.Scope)
	fmt.Println("Refresh Token: ", graphAuth.RefreshToken)

	// println("Fetched token: ", token)
	// Get the current date and the date for tomorrow
	now := time.Now()
	tomorrow := now.Add(time.Hour * 24)

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
	eventsReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", graphAuth.AccessToken))
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
	for i, event := range graphEvents.Value {
		fmt.Printf("Event %d:\n", i+1)
		fmt.Printf("Subject: %s\n", event.Subject)
		fmt.Printf("Start time: %s\n", event.Start.DateTime)
		fmt.Printf("End time: %s\n\n", event.End.DateTime)
	}

	http.Redirect(w, r, "/success", http.StatusSeeOther)
}
