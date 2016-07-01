// Copyright (c) 2016, German Neuroinformatics Node (G-Node),
//                     Adrian Stoewer <adrian.stoewer@rz.ifi.lmu.de>
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted under the terms of the BSD License. See
// LICENSE file in the root of the Project.

package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/G-Node/gin-auth/conf"
	"github.com/G-Node/gin-auth/data"
	"github.com/G-Node/gin-auth/util"
	"github.com/gorilla/mux"
)

// OAuthInfo provides information about an authorized access token
type OAuthInfo struct {
	Match util.StringSet
	Token *data.AccessToken
}

// OAuthToken gets an access token registered by an OAuthHandler.
func OAuthToken(r *http.Request) (*OAuthInfo, bool) {
	tokens.Lock()
	tok, ok := tokens.store[r]
	tokens.Unlock()

	return tok, ok
}

// Synchronized store for access tokens.
var tokens = struct {
	sync.Mutex
	store map[*http.Request]*OAuthInfo
}{store: make(map[*http.Request]*OAuthInfo)}

// OAuthHandler processes a request and extracts a bearer token from the authorization
// header. If the bearer token is valid and has a matching scope the respective AccessToken
// data can later be obtained using the OAuthToken function.
func OAuthHandler(scope ...string) func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		return oauth{Permissive: false, scope: util.NewStringSet(scope...), handler: handler}
	}
}

// OAuthHandlerPermissive processes a request and extracts a bearer token from the authorization
// header. If the bearer token is valid and has a matching scope the respective AccessToken
// data can later be obtained using the OAuthToken function.
// A permissive handler does not strictly require the presence of a bearer token. In this case
// the request is handled normally but no OAuth information is present in subsequent handlers.
func OAuthHandlerPermissive() func(http.Handler) http.Handler {
	return func(handler http.Handler) http.Handler {
		return oauth{Permissive: true, scope: util.NewStringSet(), handler: handler}
	}
}

// The actual OAuth handler
type oauth struct {
	Permissive bool
	scope      util.StringSet
	handler    http.Handler
}

// ServeHTTP implements http.Handler for oauth.
func (o oauth) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	if tokenStr := r.Header.Get("Authorization"); tokenStr != "" && strings.HasPrefix(tokenStr, "Bearer ") {
		tokenStr = strings.Trim(tokenStr[6:], " ")

		if token, ok := data.GetAccessToken(tokenStr); ok {
			match := token.Scope

			if !o.Permissive {
				match = match.Intersect(o.scope)
				if match.Len() < 1 {
					PrintErrorJSON(w, r, "Insufficient scope", http.StatusUnauthorized)
					return
				}
			}

			tokens.Lock()
			tokens.store[r] = &OAuthInfo{Match: match, Token: token}
			tokens.Unlock()

			defer func() {
				tokens.Lock()
				delete(tokens.store, r)
				tokens.Unlock()
			}()
		} else if !o.Permissive {
			PrintErrorJSON(w, r, "Invalid bearer token", http.StatusUnauthorized)
			return
		}

	} else if !o.Permissive {
		PrintErrorJSON(w, r, "No bearer token", http.StatusUnauthorized)
		return
	}

	o.handler.ServeHTTP(w, r)
}

// Authorize handles the beginning of an OAuth grant request following the schema
// of 'implicit' or 'code' grant types.
func Authorize(w http.ResponseWriter, r *http.Request) {
	param := &struct {
		ResponseType string
		ClientId     string
		RedirectURI  string
		State        string
		Scope        string
	}{}

	err := util.ReadQueryIntoStruct(r, param, false)
	if err != nil {
		PrintErrorHTML(w, r, err, 400)
		return
	}

	client, ok := data.GetClientByName(param.ClientId)
	if !ok {
		PrintErrorHTML(w, r, fmt.Sprintf("Client '%s' does not exist", param.ClientId), http.StatusBadRequest)
		return
	}

	scope := util.NewStringSet(strings.Split(param.Scope, " ")...)
	request, err := client.CreateGrantRequest(param.ResponseType, param.RedirectURI, param.State, scope)
	if err != nil {
		PrintErrorHTML(w, r, err, http.StatusBadRequest)
		return
	}

	w.Header().Add("Cache-Control", "no-store")
	http.Redirect(w, r, "/oauth/login_page?request_id="+request.Token, http.StatusFound)
}

type loginData struct {
	Login     string
	Password  string
	RequestID string
}

// LoginPage shows a page where the user can enter his credentials.
func LoginPage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query == nil || len(query) == 0 {
		PrintErrorHTML(w, r, "Query parameter 'request_id' was missing", http.StatusBadRequest)
		return
	}
	token := query.Get("request_id")

	_, ok := data.GetGrantRequest(token)
	if !ok {
		PrintErrorHTML(w, r, "Grant request does not exist", http.StatusNotFound)
		return
	}

	tmpl := conf.MakeTemplate("login.html")
	w.Header().Add("Cache-Control", "no-store")
	w.Header().Add("Content-Type", "text/html")
	err := tmpl.ExecuteTemplate(w, "layout", &loginData{RequestID: token})
	if err != nil {
		panic(err)
	}
}

// Login validates user credentials.
func Login(w http.ResponseWriter, r *http.Request) {
	param := &loginData{}
	err := util.ReadFormIntoStruct(r, param, false)
	if err != nil {
		PrintErrorHTML(w, r, err, http.StatusBadRequest)
		return
	}

	// look for existing grant request
	request, ok := data.GetGrantRequest(param.RequestID)
	if !ok {
		PrintErrorHTML(w, r, "Grant request does not exist", http.StatusNotFound)
		return
	}

	// verify login data
	account, ok := data.GetAccountByLogin(param.Login)
	if !ok {
		w.Header().Add("Cache-Control", "no-store")
		http.Redirect(w, r, "/oauth/login_page?request_id="+request.Token, http.StatusFound)
		return
	}

	ok = account.VerifyPassword(param.Password)
	if !ok {
		w.Header().Add("Cache-Control", "no-store")
		http.Redirect(w, r, "/oauth/login_page?request_id="+request.Token, http.StatusFound)
		return
	}

	request.AccountUUID = sql.NullString{String: account.UUID, Valid: true}
	err = request.Update()
	if err != nil {
		panic(err)
	}

	// if approved finish the grant request, otherwise redirect to approve page
	if request.IsApproved() {
		if request.GrantType == "code" {
			finishCodeRequest(w, r, request)
		} else {
			finishImplicitRequest(w, r, request)
		}
	} else {
		w.Header().Add("Cache-Control", "no-store")
		http.Redirect(w, r, "/oauth/approve_page?request_id="+request.Token, http.StatusFound)
	}
}

func finishCodeRequest(w http.ResponseWriter, r *http.Request, request *data.GrantRequest) {
	request.Code = sql.NullString{String: util.RandomToken(), Valid: true}
	err := request.Update()
	if err != nil {
		panic(err)
	}

	scope := url.QueryEscape(strings.Join(request.ScopeRequested.Strings(), " "))
	state := url.QueryEscape(request.State)
	url := fmt.Sprintf("%s?scope=%s&state=%s&code=%s", request.RedirectURI, scope, state, request.Code.String)

	w.Header().Add("Cache-Control", "no-store")
	http.Redirect(w, r, url, http.StatusFound)
}

func finishImplicitRequest(w http.ResponseWriter, r *http.Request, request *data.GrantRequest) {
	err := request.Delete()
	if err != nil {
		panic(err)
	}

	token := &data.AccessToken{
		Token:       util.RandomToken(),
		ClientUUID:  request.ClientUUID,
		AccountUUID: request.AccountUUID,
		Scope:       request.ScopeRequested,
	}

	err = token.Create()
	if err != nil {
		panic(err)
	}

	scope := url.QueryEscape(strings.Join(token.Scope.Strings(), " "))
	state := url.QueryEscape(request.State)
	url := fmt.Sprintf("%s?token_type=bearer&scope=%s&state=%s&access_token=%s", request.RedirectURI, scope, state, token.Token)

	w.Header().Add("Cache-Control", "no-store")
	http.Redirect(w, r, url, http.StatusFound)
}

// ApprovePage shows a page where the user can approve client access.
func ApprovePage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query == nil || len(query) == 0 {
		PrintErrorHTML(w, r, "Query parameter 'request_id' was missing", http.StatusBadRequest)
		return
	}
	token := query.Get("request_id")

	request, ok := data.GetGrantRequest(token)
	if !ok {
		PrintErrorHTML(w, r, "Grant request does not exist", http.StatusNotFound)
		return
	}
	if !request.AccountUUID.Valid {
		PrintErrorHTML(w, r, "Grant request is not authenticated", http.StatusUnauthorized)
		return
	}

	client := request.Client()
	scope := request.ScopeRequested.Difference(client.ScopeWhitelist)
	var existScope, addScope map[string]string
	if approval, ok := client.ApprovalForAccount(request.AccountUUID.String); ok && approval.Scope.Len() > 0 {
		existScope, ok = data.DescribeScope(approval.Scope)
		if !ok {
			panic("Invalid scope")
		}
		addScope, ok = data.DescribeScope(scope.Difference(approval.Scope))
		if !ok {
			panic("Invalid scope")
		}
	} else {
		addScope, ok = data.DescribeScope(scope)
		if !ok {
			panic("Invalid scope")
		}
	}

	pageData := struct {
		Client        string
		AddScope      map[string]string
		ExistingScope map[string]string
		RequestID     string
	}{client.Name, addScope, existScope, request.Token}

	tmpl := conf.MakeTemplate("approve.html")
	w.Header().Add("Cache-Control", "no-store")
	w.Header().Add("Content-Type", "text/html")
	err := tmpl.ExecuteTemplate(w, "layout", pageData)
	if err != nil {
		panic(err)
	}
}

// Approve evaluates an access approval given to a certain client.
func Approve(w http.ResponseWriter, r *http.Request) {
	param := &struct {
		Client    string
		RequestID string
		Scope     []string
	}{}
	util.ReadFormIntoStruct(r, param, true)

	request, ok := data.GetGrantRequest(param.RequestID)
	if !ok {
		PrintErrorHTML(w, r, "Grant request does not exist", http.StatusNotFound)
		return
	}

	if !request.AccountUUID.Valid {
		PrintErrorHTML(w, r, "Grant request is not authenticated", http.StatusUnauthorized)
		return
	}

	client := request.Client()

	scopeApproved := util.NewStringSet(param.Scope...)
	scopeRequired := request.ScopeRequested.Difference(client.ScopeWhitelist)
	if !scopeApproved.IsSuperset(scopeRequired) {
		PrintErrorHTML(w, r, "Requested scope was not approved", http.StatusUnauthorized)
		return
	}

	// create approval
	err := client.Approve(request.AccountUUID.String, request.ScopeRequested)
	if err != nil {
		panic(err)
	}

	// if approved finish the grant request
	if !request.IsApproved() {
		panic("Requested scope should be approved but was not")
	}

	if request.GrantType == "code" {
		finishCodeRequest(w, r, request)
	} else {
		finishImplicitRequest(w, r, request)
	}
}

type tokenResponse struct {
	TokenType    string  `json:"token_type"`
	Scope        string  `json:"scope"`
	AccessToken  string  `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
}

// Token exchanges a grant code for an access and refresh token
func Token(w http.ResponseWriter, r *http.Request) {
	// Read authorization header
	clientId, clientSecret, authorizeOk := r.BasicAuth()

	// Parse request body
	body := &struct {
		GrantType    string
		ClientId     string
		ClientSecret string
		Scope        string
		Code         string
		RefreshToken string
		Username     string
		Password     string
	}{}
	err := util.ReadFormIntoStruct(r, body, true)
	if err != nil {
		PrintErrorJSON(w, r, err, 400)
		return
	}

	// Take clientId and clientSecret from body if they are not in the header
	if !authorizeOk {
		clientId = body.ClientId
		clientSecret = body.ClientSecret
	}

	// Check client
	client, ok := data.GetClientByName(clientId)
	if !ok {
		PrintErrorJSON(w, r, "Wrong client id or client secret", http.StatusUnauthorized)
		return
	}
	if clientSecret != client.Secret {
		PrintErrorJSON(w, r, "Wrong client id or client secret", http.StatusUnauthorized)
		return
	}

	// Prepare a response depending on the grant type
	var response *tokenResponse
	switch body.GrantType {

	case "authorization_code":
		request, ok := data.GetGrantRequestByCode(body.Code)
		if !ok {
			PrintErrorJSON(w, r, "Invalid grant code", http.StatusUnauthorized)
			return
		}
		if request.ClientUUID != client.UUID {
			request.Delete()
			PrintErrorJSON(w, r, "Invalid grant code", http.StatusUnauthorized)
			return
		}

		access, refresh, err := request.ExchangeCodeForTokens()
		if err != nil {
			PrintErrorJSON(w, r, "Invalid grant code", http.StatusUnauthorized)
			return
		}

		response = &tokenResponse{
			TokenType:    "Bearer",
			Scope:        strings.Join(request.ScopeRequested.Strings(), " "),
			AccessToken:  access,
			RefreshToken: &refresh,
		}

	case "refresh_token":
		refresh, ok := data.GetRefreshToken(body.RefreshToken)
		if !ok {
			PrintErrorJSON(w, r, "Invalid refresh token", http.StatusUnauthorized)
			return
		}
		if refresh.ClientUUID != client.UUID {
			refresh.Delete()
			PrintErrorJSON(w, r, "Invalid refresh token", http.StatusUnauthorized)
			return
		}

		access := data.AccessToken{
			Token:       util.RandomToken(),
			AccountUUID: sql.NullString{String: refresh.AccountUUID, Valid: true},
			ClientUUID:  refresh.ClientUUID,
			Scope:       refresh.Scope,
		}
		err := access.Create()
		if err != nil {
			PrintErrorJSON(w, r, err, http.StatusInternalServerError)
			return
		}

		response = &tokenResponse{
			TokenType:   "Bearer",
			Scope:       strings.Join(refresh.Scope.Strings(), " "),
			AccessToken: access.Token,
		}

	case "password":
		account, ok := data.GetAccountByLogin(body.Username)
		if !ok {
			PrintErrorJSON(w, r, "Wrong username or password", http.StatusUnauthorized)
			return
		}
		if !account.VerifyPassword(body.Password) {
			PrintErrorJSON(w, r, "Wrong username or password", http.StatusUnauthorized)
			return
		}

		scope := util.NewStringSet(strings.Split(body.Scope, " ")...)
		if scope.Len() == 0 || !client.ScopeWhitelist.IsSuperset(scope) {
			PrintErrorJSON(w, r, "Invalid scope", http.StatusUnauthorized)
			return
		}

		access := data.AccessToken{
			Token:       util.RandomToken(),
			AccountUUID: sql.NullString{String: account.UUID, Valid: true},
			ClientUUID:  client.UUID,
			Scope:       scope,
		}
		err := access.Create()
		if err != nil {
			PrintErrorJSON(w, r, err, http.StatusInternalServerError)
			return
		}

		response = &tokenResponse{
			TokenType:   "Bearer",
			Scope:       strings.Join(scope.Strings(), " "),
			AccessToken: access.Token,
		}

	case "client_credentials":
		scope := util.NewStringSet(strings.Split(body.Scope, " ")...)
		if scope.Len() == 0 || !client.ScopeWhitelist.IsSuperset(scope) {
			PrintErrorJSON(w, r, "Invalid scope", http.StatusUnauthorized)
			return
		}

		access := data.AccessToken{
			Token:      util.RandomToken(),
			ClientUUID: client.UUID,
			Scope:      scope,
		}
		err := access.Create()
		if err != nil {
			PrintErrorJSON(w, r, err, http.StatusInternalServerError)
			return
		}

		response = &tokenResponse{
			TokenType:   "Bearer",
			Scope:       strings.Join(scope.Strings(), " "),
			AccessToken: access.Token,
		}

	default:
		PrintErrorJSON(w, r, fmt.Sprintf("Unsupported grant type %s", body.GrantType), http.StatusBadRequest)
		return
	}

	w.Header().Add("Cache-Control", "no-cache")
	w.Header().Add("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(response)
}

// Validate validates a token and returns information about it as JSON
func Validate(w http.ResponseWriter, r *http.Request) {
	tokenStr := mux.Vars(r)["token"]
	token, ok := data.GetAccessToken(tokenStr)
	if !ok {
		PrintErrorJSON(w, r, "The requested token does not exist", http.StatusNotFound)
		return
	}

	var login, accountUrl *string
	if token.AccountUUID.Valid {
		if account, ok := data.GetAccount(token.AccountUUID.String); ok {
			login = &account.Login
			accountUrl = new(string)
			(*accountUrl) = conf.MakeUrl("/api/accounts/%s", account.Login)
		} else {
			PrintErrorJSON(w, r, "Unable to find account associated with the request", http.StatusInternalServerError)
			return
		}
	}

	scope := strings.Join(token.Scope.Strings(), " ")
	response := &struct {
		URL        string    `json:"url"`
		JTI        string    `json:"jti"`
		EXP        time.Time `json:"exp"`
		ISS        string    `json:"iss"`
		Login      *string   `json:"login"`
		AccountURL *string   `json:"account_url"`
		Scope      string    `json:"scope"`
	}{
		URL:        conf.MakeUrl("/oauth/validate/%s", token.Token),
		JTI:        token.Token,
		EXP:        token.Expires,
		ISS:        "gin-auth",
		Login:      login,
		AccountURL: accountUrl,
		Scope:      scope,
	}

	w.Header().Add("Cache-Control", "no-cache")
	w.Header().Add("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.Encode(response)
}

// RegistrationPage displays entry fields required for the creation of a new gin account
func RegistrationPage(w http.ResponseWriter, r *http.Request) {
	tmpl := conf.MakeTemplate("registration.html")
	w.Header().Add("Cache-Control", "no-store")
	w.Header().Add("Content-Type", "text/html")
	err := tmpl.ExecuteTemplate(w, "layout", &struct{}{})
	if err != nil {
		panic(err)
	}
}

// Registration parses user entries for a new account. It will redirect back to the
// entry form, if input is invalid. If the input is correct, it will create a new account,
// send an e-mail with an activation link and redirect to the the registered page.
func Registration(w http.ResponseWriter, r *http.Request) {
	param := &struct {
		Title             string
		Firstname         string
		Middlename        string
		Lastname          string
		Login             string
		Email             string
		Emailpublic       string
		Institute         string
		Department        string
		City              string
		Country           string
		Affiliationpublic string
		Password          string
		Passwordcontrol   string
	}{}

	err := util.ReadFormIntoStruct(r, param, true)
	if err != nil {
		PrintErrorJSON(w, r, err, http.StatusInternalServerError)
		return
	}
	if r.Form.Encode() == "" {
		fmt.Println("Log: Registration: missing form")
		w.Header().Add("Cache-Control", "no-store")
		http.Redirect(w, r, "/oauth/registration_page", http.StatusFound)
		return
	}

	account := &data.Account{}

	if param.Title != "" {
		account.Title = sql.NullString{String: param.Title, Valid: true}
	}
	account.FirstName = param.Firstname
	if param.Middlename != "" {
		account.MiddleName = sql.NullString{String: param.Middlename, Valid: true}
	}
	account.LastName = param.Lastname
	account.Login = param.Login
	account.Email = param.Email
	if param.Emailpublic == "on" {
		account.IsEmailPublic = true
	}
	account.Institute = param.Institute
	account.Department = param.Department
	account.City = param.City
	account.Country = param.Country
	if param.Affiliationpublic == "on" {
		account.IsAffiliationPublic = true
	}
	account.SetPassword(param.Password)

	err = account.Create()
	if err != nil {
		fmt.Printf("Log: Registration: the following error occurred: '%s'\n", err)
		w.Header().Add("Cache-Control", "no-store")
		http.Redirect(w, r, "/oauth/registration_page", http.StatusFound)
		return
	}

	fmt.Println("Registration form was parsed")
	w.Header().Add("Cache-Control", "no-store")
	http.Redirect(w, r, "/oauth/registered_page", http.StatusFound)
}

// RegisteredPage displays information about how a newly created gin account can be activated.
func RegisteredPage(w http.ResponseWriter, r *http.Request) {
	tmpl := conf.MakeTemplate("registered.html")
	w.Header().Add("Cache-Control", "no-store")
	w.Header().Add("Content-Type", "text/html")
	err := tmpl.ExecuteTemplate(w, "layout", &struct{}{})
	if err != nil {
		panic(err)
	}
}
