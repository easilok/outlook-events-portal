package authentication

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

type CredentialStorage struct {
	Lock             sync.Mutex
	Credentials      GraphAuthentication
	ExpiresTimestamp time.Time
	Authenticated    bool
}

type GraphAuthentication struct {
	Token        string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    uint16 `json:"expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type OauthConfig struct {
	ClientID       string
	ClientSecret   string
	TenantID       string
	ServerProtocol string
	ServerHost     string
	ServerPort     int
}

type CredentialsConfig struct {
	Browser     bool
	Persist     bool
	StoragePath string
}

var oauthConfig *OauthConfig

var credentialsConfig *CredentialsConfig

var graphCredentialStorage *CredentialStorage

var refreshTokenTicker *time.Ticker

func (config *OauthConfig) RedirectURI() string {
	return fmt.Sprintf("%s://%s:%d/callback", config.ServerProtocol, config.ServerHost, config.ServerPort)
}

func (config *OauthConfig) BaseURL() string {
	return fmt.Sprintf("%s://%s:%d", config.ServerProtocol, config.ServerHost, config.ServerPort)
}

func (credentials *CredentialStorage) Run(oauth *OauthConfig, appConfig *CredentialsConfig) {

	oauthConfig = oauth
	credentialsConfig = appConfig
	graphCredentialStorage = credentials

	http.HandleFunc("/home", homeHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/success", successHandler)
	http.HandleFunc("/error", errorHandler)
	http.HandleFunc("/callback", callbackHandler)

	// If credentials are empty or RefreshToken is empty
	if len((*graphCredentialStorage).Credentials.RefreshToken) > 0 {
		if err := refreshTokenHandler(); err == nil {
			// If token refresh succeeds, start the refresh manager
			go graphCredentialStorage.refreshTokenManager()
			return
		}
	}
	// The browser can connect now because the listening socket is open.
	openLoginPage()
}

func (credentials *CredentialStorage) refreshTokenManager() {
	// This manager is a infinite loop only stopping if token refresh results in error
	for {
		// Start by saving the current fetched credentials
		if credentialsConfig.Persist {
			go credentials.persistCredentials(credentialsConfig.StoragePath)
		}

		// Extracting expiring time with lock
		credentials.Lock.Lock()
		// timeUntilRefresh := time.Duration(credentials.Credentials.ExpiresIn-60) * time.Second
		timeUntilRefresh := 10 * time.Second
		credentials.Lock.Unlock()

		<-time.After(timeUntilRefresh)
		// <-time.After(30 * time.Second)
		if err := refreshTokenHandler(); err != nil {
			// If token refresh results in error, stop the refresh manager
			return
		}
	}
}

func (credentials *CredentialStorage) persistCredentials(path string) {
	if len(path) == 0 {
		return
	}

	buf := new(bytes.Buffer)

	credentials.Lock.Lock()
	err := toml.NewEncoder(buf).Encode(credentials.Credentials)
	credentials.Lock.Unlock()

	if err != nil {
		fmt.Printf("Error encoding current credentials: %s\n", err.Error())
		return
	}

	// create the file
	f, err := os.Create(filepath.Join(path, "credentials.toml"))
	if err != nil {
		fmt.Printf("Error creating file to save credentials: %s\n", err.Error())
		return
	}
	// close the file with defer
	defer f.Close()

	// write a string
	f.WriteString(buf.String())
}

func openLoginPage() {
	if credentialsConfig.Browser {
		browserOpenErr := exec.Command("open", fmt.Sprintf("%s/home", oauthConfig.BaseURL())).Start()
		if browserOpenErr != nil {
			fmt.Printf("Error opening browser to login: %s\n", browserOpenErr.Error())
		}
	}
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	// Display a link to the login page
	fmt.Fprint(w, "<a href='/login'>Login</a>")
}

func successHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "<p>Login success. You can not close this tab.</p>")
}

func errorHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "<p>Error on login to Microsoft Graph API.</p><br/><p>Try again at <a href='/home'>Home</a></p>")
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	// Redirect the user to the Microsoft login page
	url := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/authorize?client_id=%s&response_type=code&redirect_uri=%s&scope=Calendars.Read", oauthConfig.TenantID, oauthConfig.ClientID, oauthConfig.RedirectURI())
	http.Redirect(w, r, url, http.StatusFound)
}

func refreshTokenHandler() error {
	// Use object lock for multithread access
	graphCredentialStorage.Lock.Lock()
	defer graphCredentialStorage.Lock.Unlock()

	// Use the authorization code to acquire an access token
	data := url.Values{
		"client_id":     {oauthConfig.ClientID},
		"scope":         {"https://graph.microsoft.com/.default offline_access"},
		"refresh_token": {graphCredentialStorage.Credentials.RefreshToken},
		"redirect_uri":  {oauthConfig.RedirectURI()},
		"grant_type":    {"refresh_token"},
		"client_secret": {oauthConfig.ClientSecret},
	}

	resp, err := http.PostForm(fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", oauthConfig.TenantID), data)
	if err != nil {
		fmt.Println("Error refresh token:", err)
		graphCredentialStorage.Credentials = GraphAuthentication{}
		graphCredentialStorage.Authenticated = false
		openLoginPage()
		return err
	}

	var graphAuth GraphAuthentication

	err = json.NewDecoder(resp.Body).Decode(&graphAuth)
	if err != nil {
		fmt.Println("Error decoding refresh token response: ", err)
		graphCredentialStorage.Credentials = GraphAuthentication{}
		graphCredentialStorage.Authenticated = false
		openLoginPage()
		return err
	}

	fmt.Println("Token refresh succeeded")
	graphCredentialStorage.Credentials.Token = graphAuth.Token
	graphCredentialStorage.Credentials.AccessToken = graphAuth.AccessToken
	graphCredentialStorage.Credentials.ExpiresIn = graphAuth.ExpiresIn
	graphCredentialStorage.ExpiresTimestamp = time.Now().Add(time.Second * time.Duration(graphAuth.ExpiresIn))
	graphCredentialStorage.Credentials.RefreshToken = graphAuth.RefreshToken
	graphCredentialStorage.Credentials.Scope = graphAuth.Scope
	graphCredentialStorage.Authenticated = true
	return nil

}

func callbackHandler(w http.ResponseWriter, r *http.Request) {
	// Get the authorization code from the query parameter
	code := r.URL.Query().Get("code")

	// Use the authorization code to acquire an access token
	data := url.Values{
		"client_id":     {oauthConfig.ClientID},
		"scope":         {"https://graph.microsoft.com/.default offline_access"},
		"code":          {code},
		"redirect_uri":  {oauthConfig.RedirectURI()},
		"grant_type":    {"authorization_code"},
		"client_secret": {oauthConfig.ClientSecret},
	}

	resp, err := http.PostForm(fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", oauthConfig.TenantID), data)
	if err != nil {
		fmt.Println("Error acquiring token:", err)
		http.Redirect(w, r, "/error", http.StatusBadRequest)
		return
	}

	var graphAuth GraphAuthentication

	err = json.NewDecoder(resp.Body).Decode(&graphAuth)
	if err != nil {
		fmt.Println("Error decoding graph token response: ", err.Error())
		http.Redirect(w, r, "/error", http.StatusBadRequest)
		return
	}

	fmt.Println("Token received")
	// Use object lock for multithread access
	graphCredentialStorage.Lock.Lock()

	graphCredentialStorage.Credentials.Token = graphAuth.Token
	graphCredentialStorage.Credentials.AccessToken = graphAuth.AccessToken
	graphCredentialStorage.Credentials.ExpiresIn = graphAuth.ExpiresIn
	graphCredentialStorage.ExpiresTimestamp = time.Now().Add(time.Second * time.Duration(graphAuth.ExpiresIn))
	graphCredentialStorage.Credentials.RefreshToken = graphAuth.RefreshToken
	graphCredentialStorage.Credentials.Scope = graphAuth.Scope
	graphCredentialStorage.Authenticated = true

	graphCredentialStorage.Lock.Unlock()

	http.Redirect(w, r, "/success", http.StatusSeeOther)

	// start timer to call refreshTokenHandler when access token is about to expire
	go graphCredentialStorage.refreshTokenManager()
}
