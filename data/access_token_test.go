// Copyright (c) 2016, German Neuroinformatics Node (G-Node),
//                     Adrian Stoewer <adrian.stoewer@rz.ifi.lmu.de>
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted under the terms of the BSD License. See
// LICENSE file in the root of the Project.

package data

import (
	"testing"
	"time"

	"database/sql"
	"github.com/G-Node/gin-auth/util"
)

const (
	accessTokenAlice = "3N7MP7M7"
	accessTokenBob   = "LJ3W7ZFK" // is expired
)

func TestListAccessTokens(t *testing.T) {
	defer util.FailOnPanic(t)
	InitTestDb(t)

	accessTokens := ListAccessTokens()
	if len(accessTokens) != 2 {
		t.Error("Exactly two access tokens expected in slice.")
	}
}

func TestGetAccessToken(t *testing.T) {
	defer util.FailOnPanic(t)
	InitTestDb(t)

	tok, ok := GetAccessToken(accessTokenAlice)
	if !ok {
		t.Error("Access token does not exist")
	}
	if !tok.AccountUUID.Valid || tok.AccountUUID.String != uuidAlice {
		t.Errorf("AccountUUID was expectd to be '%s'", uuidAlice)
	}

	_, ok = GetAccessToken("doesNotExist")
	if ok {
		t.Error("Access token should not exist")
	}

	_, ok = GetAccessToken(accessTokenBob)
	if ok {
		t.Error("Expired access token should not be retrieved.")
	}
}

func TestCreateAccessToken(t *testing.T) {
	InitTestDb(t)

	token := util.RandomToken()
	fresh := AccessToken{
		Token:       token,
		Scope:       util.NewStringSet("foo-read", "foo-write"),
		Expires:     time.Now().Add(time.Hour * 12),
		ClientUUID:  uuidClientGin,
		AccountUUID: sql.NullString{String: uuidAlice, Valid: true},
	}

	err := fresh.Create()
	if err != nil {
		t.Error(err)
	}

	check, ok := GetAccessToken(token)
	if !ok {
		t.Error("Token does not exist")
	}
	if !check.AccountUUID.Valid || check.AccountUUID.String != uuidAlice {
		t.Errorf("AccountUUID is supposed to be '%s'", uuidAlice)
	}
	if !check.Scope.Contains("foo-read") {
		t.Error("Scope should contain 'foo-read'")
	}
	if !check.Scope.Contains("foo-write") {
		t.Error("Scope should contain 'foo-write'")
	}

	token = util.RandomToken()
	fresh = AccessToken{
		Token:      token,
		Scope:      util.NewStringSet("foo-read"),
		Expires:    time.Now().Add(time.Hour * 12),
		ClientUUID: uuidClientGin,
	}

	err = fresh.Create()
	if err != nil {
		t.Error(err)
	}

	check, ok = GetAccessToken(token)
	if !ok {
		t.Error("Token does not exist")
	}
	if !check.Scope.Contains("foo-read") {
		t.Error("Scope should contain 'foo-read'")
	}
}

func TestAccessTokenUpdateExpirationTime(t *testing.T) {
	InitTestDb(t)

	tok, ok := GetAccessToken(accessTokenAlice)
	if !ok {
		t.Error("Access token does not exist.")
	}

	if time.Since(tok.Expires) > 0 {
		t.Error("Access token should not be expired.")
	}

	oldExpired := tok.Expires
	err := tok.UpdateExpirationTime()
	if err != nil {
		t.Errorf("Error updating expiration time: %v\n", err)
	}
	if !tok.Expires.After(oldExpired) {
		t.Error("Access token expired was not properly updated.")
	}

	check, ok := GetAccessToken(accessTokenAlice)
	if !ok {
		t.Error("Access token does not exist.")
	}
	if time.Since(check.Expires) > 0 {
		t.Error("Token should not be expired.")
	}
}

func TestAccessTokenDelete(t *testing.T) {
	InitTestDb(t)

	tok, ok := GetAccessToken(accessTokenAlice)
	if !ok {
		t.Error("Access token does not exist")
	}

	err := tok.Delete()
	if err != nil {
		t.Error(err)
	}

	_, ok = GetAccessToken(accessTokenAlice)
	if ok {
		t.Error("Access token should not exist")
	}
}
