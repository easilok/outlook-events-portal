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
	"runtime"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

type CredentialStorage struct {
	lock             sync.Mutex
	credentials      GraphAuthentication
	expiresTimestamp time.Time
	authenticated    bool
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

type Logger interface {
	Debug(args ...interface{})
	Info(args ...interface{})
	Warn(args ...interface{})
	Error(args ...interface{})
	Fatal(args ...interface{})
}

var oauthConfig OauthConfig

var credentialsConfig CredentialsConfig

var graphCredentialStorage CredentialStorage

var refreshTokenManagerRunning = false

var logger Logger

func (config *OauthConfig) RedirectURI() string {
	return fmt.Sprintf("%s://%s:%d/callback", config.ServerProtocol, config.ServerHost, config.ServerPort)
}

func (config *OauthConfig) BaseURL() string {
	return fmt.Sprintf("%s://%s:%d", config.ServerProtocol, config.ServerHost, config.ServerPort)
}

func loadExistingCredentials() {
	// Initialize variable
	graphCredentialStorage = CredentialStorage{}
	// Only load if persistent storage is enabled
	if !credentialsConfig.Persist {
		return
	}
	credentialsContent, err := os.ReadFile(filepath.Join(credentialsConfig.StoragePath, "credentials.toml"))
	if err != nil {
		logger.Error(fmt.Sprintf("Error loading existing credentials file: %s", err.Error()))
	}

	_, err = toml.Decode(string(credentialsContent), &graphCredentialStorage.credentials)
	if err != nil {
		logger.Error(fmt.Sprintf("Error decoding existing credentials file: %s", err.Error()))
	}
}

func Run(oauth *OauthConfig, appConfig *CredentialsConfig, log Logger) *CredentialStorage {

	oauthConfig = *oauth
	credentialsConfig = *appConfig
	loadExistingCredentials()
	logger = log

	http.HandleFunc("/home", homeHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/success", successHandler)
	http.HandleFunc("/error", errorHandler)
	http.HandleFunc("/callback", callbackHandler)

	// If credentials are empty or RefreshToken is empty
	if len(graphCredentialStorage.credentials.RefreshToken) > 0 {
		if err := refreshTokenHandler(); err == nil {
			// If token refresh succeeds, start the refresh manager
			go graphCredentialStorage.refreshTokenManager()
			return nil
		}
	}
	// The browser can connect now because the listening socket is open.
	openLoginPage()

	return &graphCredentialStorage
}

func (credentials *CredentialStorage) GetAccessToken() (string, bool) {
	credentials.lock.Lock()
	Authenticated := credentials.authenticated
	accessToken := credentials.credentials.AccessToken
	credentials.lock.Unlock()

	return accessToken, Authenticated
}

func (credentials *CredentialStorage) refreshTokenManager() {
	if refreshTokenManagerRunning {
		// Don't start another manager
		return
	}
	refreshTokenManagerRunning = true
	// This manager is a infinite loop only stopping if token refresh results in error
	for {
		// Start by saving the current fetched credentials
		if credentialsConfig.Persist {
			credentials.persistCredentials(credentialsConfig.StoragePath)
		}

		// Extracting expiring time with lock
		credentials.lock.Lock()
		timeUntilRefresh := time.Duration(credentials.credentials.ExpiresIn-60) * time.Second
		// timeUntilRefresh := 10 * time.Second
		credentials.lock.Unlock()

		time.Sleep(timeUntilRefresh)

		logger.Debug(fmt.Sprintf("***Credentials Manager: currently running goroutines: %d***", runtime.NumGoroutine()))

		// <-time.After(30 * time.Second)
		if err := refreshTokenHandler(); err != nil {
			// If token refresh results in error, stop the refresh manager
			refreshTokenManagerRunning = false
			return
		}
	}
}

func (credentials *CredentialStorage) persistCredentials(path string) {
	if len(path) == 0 {
		return
	}

	buf := new(bytes.Buffer)

	credentials.lock.Lock()
	err := toml.NewEncoder(buf).Encode(credentials.credentials)
	credentials.lock.Unlock()

	if err != nil {
		fmt.Printf("Error encoding current credentials: %s\n", err.Error())
		return
	}

	// create the file
	f, err := os.Create(filepath.Join(path, "credentials.toml"))
	if err != nil {
		logger.Fatal(fmt.Sprintf("Error creating file to save credentials: %s\n", err.Error()))
		return
	}
	// write a string
	_, err = f.WriteString(buf.String())
	if err != nil {
		logger.Fatal(fmt.Sprintf("Error writing file to save credentials: %s\n", err.Error()))
	}
	// close the file with defer
	err = f.Close()
	if err != nil {
		logger.Fatal(fmt.Sprintf("Error closing file to save credentials: %s\n", err.Error()))
	}

}

func openLoginPage() {
	if credentialsConfig.Browser {
		browserOpenErr := exec.Command("open", fmt.Sprintf("%s/home", oauthConfig.BaseURL())).Start()
		if browserOpenErr != nil {
			logger.Error(fmt.Sprintf("Error opening browser to login: %s\n", browserOpenErr.Error()))
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
	graphCredentialStorage.lock.Lock()
	defer graphCredentialStorage.lock.Unlock()

	// Use the authorization code to acquire an access token
	data := url.Values{
		"client_id":     {oauthConfig.ClientID},
		"scope":         {"https://graph.microsoft.com/.default offline_access"},
		"refresh_token": {graphCredentialStorage.credentials.RefreshToken},
		"redirect_uri":  {oauthConfig.RedirectURI()},
		"grant_type":    {"refresh_token"},
		"client_secret": {oauthConfig.ClientSecret},
	}

	resp, err := http.PostForm(fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", oauthConfig.TenantID), data)
	if err != nil {
		logger.Error(fmt.Sprintf("Error refresh token: %s", err.Error()))
		return nil
	}

	if resp.StatusCode != 200 {
		logger.Warn(fmt.Sprintf("Received status %d from server. Body is: %s", resp.StatusCode, resp.Body))
		graphCredentialStorage.credentials = GraphAuthentication{}
		graphCredentialStorage.authenticated = false
		openLoginPage()
		return nil
	}

	var graphAuth GraphAuthentication

	err = json.NewDecoder(resp.Body).Decode(&graphAuth)
	resp.Body.Close()
	if err != nil {
		logger.Error(fmt.Sprintf("Error decoding refresh token response: %s", err.Error()))
		graphCredentialStorage.credentials = GraphAuthentication{}
		graphCredentialStorage.authenticated = false
		openLoginPage()
		return err
	}

	logger.Info("Token refresh succeeded")
	graphCredentialStorage.credentials.Token = graphAuth.Token
	graphCredentialStorage.credentials.AccessToken = graphAuth.AccessToken
	graphCredentialStorage.credentials.ExpiresIn = graphAuth.ExpiresIn
	graphCredentialStorage.expiresTimestamp = time.Now().Add(time.Second * time.Duration(graphAuth.ExpiresIn))
	graphCredentialStorage.credentials.RefreshToken = graphAuth.RefreshToken
	graphCredentialStorage.credentials.Scope = graphAuth.Scope
	graphCredentialStorage.authenticated = true
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
		logger.Error(fmt.Sprintf("Error acquiring token: %s", err.Error()))
		http.Redirect(w, r, "/error", http.StatusBadRequest)
		return
	}

	var graphAuth GraphAuthentication

	err = json.NewDecoder(resp.Body).Decode(&graphAuth)
	resp.Body.Close()
	if err != nil {
		logger.Error(fmt.Sprintf("Error decoding graph token response: %s", err.Error()))
		http.Redirect(w, r, "/error", http.StatusBadRequest)
		return
	}

	logger.Info("Token received")
	// Use object lock for multithread access
	graphCredentialStorage.lock.Lock()

	graphCredentialStorage.credentials.Token = graphAuth.Token
	graphCredentialStorage.credentials.AccessToken = graphAuth.AccessToken
	graphCredentialStorage.credentials.ExpiresIn = graphAuth.ExpiresIn
	graphCredentialStorage.expiresTimestamp = time.Now().Add(time.Second * time.Duration(graphAuth.ExpiresIn))
	graphCredentialStorage.credentials.RefreshToken = graphAuth.RefreshToken
	graphCredentialStorage.credentials.Scope = graphAuth.Scope
	graphCredentialStorage.authenticated = true

	graphCredentialStorage.lock.Unlock()

	http.Redirect(w, r, "/success", http.StatusSeeOther)

	// start timer to call refreshTokenHandler when access token is about to expire
	go graphCredentialStorage.refreshTokenManager()
}
