package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"

	// "os"
	"strconv"
	"time"

	"recallo/internals/logger"
	"recallo/internals/models"
	"recallo/internals/utils"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
)

var (
	githubOAuthConfig *oauth2.Config
	frontendURL       = "https://recallo.tech"
)

func InitOAuth(clientID, clientSecret, redirectURL string, fURL string) {
	frontendURL = fURL
	githubOAuthConfig = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       []string{"user:email", "read:user"},
		Endpoint:     github.Endpoint,
		RedirectURL:  redirectURL,
	}
}

func generateStateOauthCookie(w http.ResponseWriter) string {
	b := make([]byte, 32)
	rand.Read(b)
	state := base64.URLEncoding.EncodeToString(b)

	cookie := http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Expires:  time.Now().Add(10 * time.Minute), // 10 minutes
		HttpOnly: true,
		Path:     "/",
		SameSite: http.SameSiteLaxMode, // Adjust to Strict in production over HTTPS
	}
	http.SetCookie(w, &cookie)

	return state
}

func HandleBeginGithubAuth(w http.ResponseWriter, r *http.Request) {
	state := generateStateOauthCookie(w)
	url := githubOAuthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// HandleGithubAuthCallback processes the callback from GitHub
func HandleGithubAuthCallback(w http.ResponseWriter, r *http.Request) {
	oauthState, err := r.Cookie("oauth_state")
	if err != nil {
		logger.App.Printf("[OAUTH] error=missing_state_cookie")
		http.Redirect(w, r, frontendURL+"/login?error=invalid_state", http.StatusTemporaryRedirect)
		return
	}

	if r.FormValue("state") != oauthState.Value {
		logger.App.Printf("[OAUTH] error=state_mismatch")
		http.Redirect(w, r, frontendURL+"/login?error=invalid_state", http.StatusTemporaryRedirect)
		return
	}

	code := r.FormValue("code")
	token, err := githubOAuthConfig.Exchange(context.Background(), code)
	if err != nil {
		logger.App.Printf("[OAUTH] error=code_exchange_failed err=%v", err)
		http.Redirect(w, r, frontendURL+"/login?error=exchange_failed", http.StatusTemporaryRedirect)
		return
	}

	// Fetch user details from GitHub
	client := githubOAuthConfig.Client(context.Background(), token)
	resp, err := client.Get("https://api.github.com/user")
	if err != nil || resp.StatusCode != 200 {
		logger.App.Printf("[OAUTH] error=failed_fetch_user err=%v", err)
		http.Redirect(w, r, frontendURL+"/login?error=fetch_failed", http.StatusTemporaryRedirect)
		return
	}
	defer resp.Body.Close()

	var ghUser struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"` // Could be empty, might need to fetch /user/emails
	}
	if err := json.NewDecoder(resp.Body).Decode(&ghUser); err != nil {
		logger.App.Printf("[OAUTH] error=failed_decode_user err=%v", err)
		http.Redirect(w, r, frontendURL+"/login?error=decode_failed", http.StatusTemporaryRedirect)
		return
	}

	// 1. Find or create the user in our database
	user, err := models.GetUserByEmail(ghUser.Email)
	if err != nil {
		// Create a new user (with a dummy password since they use OAuth)
		// Note: CreateUser takes (name, email, password)
		dummyPassword, _ := utils.HashPassword(base64.URLEncoding.EncodeToString([]byte(ghUser.Login + "dummy")))
		user, err = models.CreateUser(ghUser.Name, ghUser.Email, dummyPassword)
		if err != nil {
			logger.App.Printf("[OAUTH] error=failed_create_user err=%v", err)
			http.Redirect(w, r, frontendURL+"/login?error=user_creation_failed", http.StatusTemporaryRedirect)
			return
		}
	}

	// 2. Save the encrypted OAuth tokens for background jobs/refresh
	err = models.SaveOAuthToken(user.ID, "github", strconv.FormatInt(ghUser.ID, 10), token)
	if err != nil {
		logger.App.Printf("[OAUTH] warning=failed_save_oauth_token err=%v", err)
	}

	// 3. Generate our internal app JWT session tokens (same as login.go)
	platform := "web" // default for OAuth web flow
	appAccessToken, err := utils.GenerateJwtToken(user.ID, user.Name, platform)
	if err != nil {
		http.Redirect(w, r, frontendURL+"/login?error=jwt_failed", http.StatusTemporaryRedirect)
		return
	}

	appRefreshToken, err := utils.GenerateRefreshToken()
	if err != nil {
		http.Redirect(w, r, frontendURL+"/login?error=jwt_failed", http.StatusTemporaryRedirect)
		return
	}

	err = utils.UpdateRefreshToken(user.ID, platform, appRefreshToken)
	if err != nil {
		http.Redirect(w, r, frontendURL+"/login?error=token_update_failed", http.StatusTemporaryRedirect)
		return
	}

	// 4. Redirect to frontend with the tokens (or set them as cookies here)
	// For Next.js/React it's common to redirect to a specific success route that consumes these query params
	// and stores them securely on the client, then redirects to the dashboard.
	redirectURL := frontendURL + "/oauth/success?access_token=" + appAccessToken + "&refresh_token=" + appRefreshToken
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect)
}
