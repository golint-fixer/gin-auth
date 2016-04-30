// Copyright (c) 2016, German Neuroinformatics Node (G-Node),
//                     Adrian Stoewer <adrian.stoewer@rz.ifi.lmu.de>
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted under the terms of the BSD License. See
// LICENSE file in the root of the Project.

package data

import (
	"database/sql"
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/G-Node/gin-auth/util"
	"github.com/jmoiron/sqlx"
	"github.com/pborman/uuid"
	"gopkg.in/yaml.v2"
)

// Client object stored in the database
type Client struct {
	UUID             string
	Name             string
	Secret           string
	ScopeProvidedMap map[string]string
	RedirectURIs     util.StringSet
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ListClients returns all registered OAuth clients ordered by name
func ListClients() []Client {
	// TODO remove once this is replaced by private listClients method
	// and the tests have been modified.
	const q = `SELECT * FROM Clients ORDER BY name`

	clients := make([]Client, 0)
	err := database.Select(&clients, q)
	if err != nil {
		panic(err)
	}

	return clients
}

// listClientUUIDs returns a StringSet of the UUIDs of clients currently
// in the database.
func listClientUUIDs() util.StringSet {
	const q = "SELECT uuid FROM Clients"

	clients := make([]string, 0)
	err := database.Select(&clients, q)
	if err != nil {
		panic(err)
	}

	return util.NewStringSet(clients...)
}

// GetClient returns an OAuth client with a given uuid.
// Returns false if no client with a matching uuid can be found.
func GetClient(uuid string) (*Client, bool) {
	const q = `SELECT * FROM Clients WHERE uuid=$1`
	return getClient(q, uuid)
}

// GetClientByName returns an OAuth client with a given client name.
// Returns false if no client with a matching name can be found.
func GetClientByName(name string) (*Client, bool) {
	const q = `SELECT * FROM Clients WHERE name=$1`
	return getClient(q, name)
}

func getClient(q, parameter string) (*Client, bool) {
	const qScope = `SELECT name, description FROM ClientScopeProvided WHERE clientUUID = $1`

	client := &Client{ScopeProvidedMap: make(map[string]string)}
	err := database.Get(client, q, parameter)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false
		}
		panic(err)
	}

	scope := []struct {
		Name        string
		Description string
	}{}
	err = database.Select(&scope, qScope, client.UUID)
	if err != nil {
		panic(err)
	}
	for _, s := range scope {
		client.ScopeProvidedMap[s.Name] = s.Description
	}

	return client, true
}

// CheckScope checks whether a certain scope exists by searching
// through all provided scopes from all registered clients.
func CheckScope(scope util.StringSet) bool {
	const q = `SELECT name FROM ClientScopeProvided`

	if scope.Len() == 0 {
		return false
	}

	check := []string{}
	err := database.Select(&check, q)
	if err != nil {
		panic(err)
	}

	global := util.NewStringSet(check...)
	return global.IsSuperset(scope)
}

// DescribeScope turns a scope into a map of names to descriptions.
// If the map is complete the second return value is true.
func DescribeScope(scope util.StringSet) (map[string]string, bool) {
	const q = `SELECT name, description FROM ClientScopeProvided`

	desc := make(map[string]string)
	if scope.Len() == 0 {
		return desc, false
	}

	data := []struct {
		Name        string
		Description string
	}{}

	err := database.Select(&data, q)
	if err != nil {
		panic(err)
	}

	names := make([]string, len(data))
	for i, d := range data {
		names[i] = d.Name
		desc[d.Name] = d.Description
	}
	global := util.NewStringSet(names...)

	return desc, global.IsSuperset(scope)
}

// ScopeProvided the scope provided by this client as a StringSet.
// The scope is extracted from the clients ScopeProvidedMap.
func (client *Client) ScopeProvided() util.StringSet {
	scope := make([]string, 0, len(client.ScopeProvidedMap))
	for s := range client.ScopeProvidedMap {
		scope = append(scope, s)
	}
	return util.NewStringSet(scope...)
}

// Create stores a new client in the database.
func (client *Client) create(tx *sqlx.Tx) error {
	const q = `INSERT INTO Clients (uuid, name, secret, redirectURIs, createdAt, updatedAt)
	           VALUES ($1, $2, $3, $4, now(), now())
	           RETURNING *`
	const qScope = `INSERT INTO ClientScopeProvided (clientUUID, name, description)
					VALUES ($1, $2, $3)`

	if client.UUID == "" {
		client.UUID = uuid.NewRandom().String()
	}

	err := tx.Get(client, q, client.UUID, client.Name, client.Secret, client.RedirectURIs)
	if err == nil {
		for k, v := range client.ScopeProvidedMap {
			_, err = tx.Exec(qScope, client.UUID, k, v)
			if err != nil {
				break
			}
		}
	}

	return err
}

// Create stores a new client in the database.
func (client *Client) Create() error {
// TODO remove after refactoring of tests that use this method
	const q = `INSERT INTO Clients (uuid, name, secret, redirectURIs, createdAt, updatedAt)
	           VALUES ($1, $2, $3, $4, now(), now())
	           RETURNING *`
	const qScope = `INSERT INTO ClientScopeProvided (clientUUID, name, description)
					VALUES ($1, $2, $3)`

	if client.UUID == "" {
		client.UUID = uuid.NewRandom().String()
	}

	tx := database.MustBegin()
	err := tx.Get(client, q, client.UUID, client.Name, client.Secret, client.RedirectURIs)
	if err != nil {
		tx.Rollback()
		return err
	}
	for k, v := range client.ScopeProvidedMap {
		_, err = tx.Exec(qScope, client.UUID, k, v)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}


// ApprovalForAccount gets a client approval for this client which was
// approved for a specific account.
func (client *Client) ApprovalForAccount(accountUUID string) (*ClientApproval, bool) {
	const q = `SELECT * FROM ClientApprovals WHERE clientUUID = $1 AND accountUUID = $2`

	approval := &ClientApproval{}
	err := database.Get(approval, q, client.UUID, accountUUID)
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}
	return approval, err == nil
}

// Approve creates a new client approval or extends an existing approval, such that the
// given scope is is approved for the given account.
func (client *Client) Approve(accountUUID string, scope util.StringSet) (err error) {
	if !CheckScope(scope) {
		return errors.New("Invalid scope")
	}

	approval, ok := client.ApprovalForAccount(accountUUID)
	if ok {
		// approval exists
		if !approval.Scope.IsSuperset(scope) {
			approval.Scope = approval.Scope.Union(scope)
			err = approval.Update()
		}
	} else {
		// create new approval
		approval = &ClientApproval{
			ClientUUID:  client.UUID,
			AccountUUID: accountUUID,
			Scope:       scope,
		}
		err = approval.Create()
	}
	return err
}

// CreateGrantRequest check whether response type, redirect URI and scope are valid and creates a new
// grant request for this client.
func (client *Client) CreateGrantRequest(responseType, redirectURI, state string, scope util.StringSet) (*GrantRequest, error) {
	if !(responseType == "code" || responseType == "token") {
		return nil, errors.New("Response type expected to be 'code' or 'token'")
	}
	if !client.RedirectURIs.Contains(redirectURI) {
		return nil, fmt.Errorf("Redirect URI invalid: '%s'", redirectURI)
	}
	if !CheckScope(scope) {
		return nil, errors.New("Invalid scope")
	}

	request := &GrantRequest{
		GrantType:      responseType,
		RedirectURI:    redirectURI,
		State:          state,
		ScopeRequested: scope,
		ClientUUID:     client.UUID}
	err := request.Create()

	return request, err
}

// Delete removes an existing client from the database
func (client *Client) Delete() error {
	const q = `DELETE FROM Clients c WHERE c.uuid=$1`

	_, err := database.Exec(q, client.UUID)
	return err
}

// delete removes and existing client from the database using a transaction.
func (client *Client) delete(tx *sqlx.Tx) error {
	const q = `DELETE FROM Clients c WHERE c.uuid=$1`

	_, err := tx.Exec(q, client.UUID)

	return err
}

func (client *Client) deleteScope(tx *sqlx.Tx) error {
	const q = `DELETE FROM ClientScopeProvided WHERE clientuuid=$1`

	_, err := tx.Exec(q, client.UUID)

	return err
}

func (client *Client) createScope(tx *sqlx.Tx) error {
	const qScope = `INSERT INTO ClientScopeProvided (clientUUID, name, description)
					VALUES ($1, $2, $3)`

	var err error
	for k, v := range client.ScopeProvidedMap {
		_, err = tx.Exec(qScope, client.UUID, k, v)
		if err != nil {
			break
		}
	}
	return err
}

func (client *Client) update(tx *sqlx.Tx) error {
	const q = `UPDATE Clients c (name, secret, redirectURIs, updatedAt)
	           VALUES ($2, $3, $4, now())
	           WHERE c.uuid=$1`

	err := client.deleteScope(tx)
	if err != nil {
		return err
	}

	_, err = tx.Exec(q, client.UUID, client.Name, client.Secret, client.RedirectURIs, time.Now())
	if err != nil {
		return err
	}

	if len(client.ScopeProvidedMap) > 0 {
		err = client.createScope(tx)
	}

	return err
}

type initClient struct{
	UUID          	string				`yaml:"UUID"`
	Name          	string				`yaml:"Name"`
	Secret        	string				`yaml:"Secret"`
	ScopeProvided	map[string]string	`yaml:"ScopeProvided"`
	RedirectURIs	[]string			`yaml:"RedirectURIs"`
}

// InitClients loads client information from a yaml configuration file
// and updates the corresponding entries in the database.
func InitClients(path string) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		panic(err)
	}

	var confClients []initClient

	err = yaml.Unmarshal(content, &confClients)
	if err != nil {
		panic(err)
	}

	ClientToDatabase(confClients)
}

// ClientToDatabase writes client information from an initClient slice to the database.
func ClientToDatabase(confClients []initClient) {
	// TODO rename
	fmt.Printf("[Dev] Size: %d, Content: %v\n", len(confClients), confClients)

	clientIDs := make([]string, len(confClients), len(confClients))
	for i, v := range confClients {
		fmt.Printf("[Dev] Index: %d, UUID: '%s'\n", i, v.UUID)
		clientIDs[i] = v.UUID
	}

	confClientIDs := util.NewStringSet(clientIDs...)

	// TODO check how to lock the database, so that no changes can be made
	// after the UUIDs of the clients in the database have been collected
	// until the database transactions have been successfully executed.

	dbClientIDs := listClientUUIDs()
	fmt.Printf("[Dev] Size dbClients: %d\n\tClient: %v\n", len(dbClientIDs), dbClientIDs)

	fmt.Printf("[Dev] Get to be removed.")
	removeDbClients := Difference(confClientIDs, dbClientIDs)

	// Get transaction
	tx := database.MustBegin()

	var err error
	//1 remove all clients from the database, that are not in the config clients list
	if len(removeDbClients) > 0 {
		for currUUID := range removeDbClients {
			fmt.Printf("[Dev] Remove ID: '%s'\n", currUUID)
			remClient, clientExists := GetClient(currUUID)
			if clientExists {
				fmt.Printf("[Dev] Remove actual client: %v\n", remClient)
				//err = remClient.delete(&tx)
				if err != nil {
					break
				}
			}
		}
	}

	//2 loop through all config client entries, check if already existent in the database
	for ind, cl := range confClients {
		// TODO better mapping of confClient to actual client
		var currClient Client
		currClient.UUID = cl.UUID
		currClient.Name = cl.Name
		currClient.Secret = cl.Secret
		currClient.ScopeProvidedMap = cl.ScopeProvided
		currClient.RedirectURIs = util.NewStringSet(cl.RedirectURIs...)
		fmt.Printf("[Dev] curr new client: %v\n", currClient)

		fmt.Printf("[Dev] Handle client #%d, ID: '%s'\n", ind, currClient.UUID)
		if dbClientIDs.Contains(currClient.UUID) {
			//2.1 if already exist, update clients
			//2.1.1 check if config has scopes; if yes remove all entries from db and insert new ones
			fmt.Printf("[Dev]\t Client in db update\n")
			//err = currClient.update(&tx)
		} else {
			//2.2 if no, insert clients && clientScopeProvided
			fmt.Printf("[Dev]\t Client not yet in db, insert\n")
			//err = currClient.create(&tx)
		}
		if err != nil {
			break
		}
	}

	if err != nil {
		tx.Rollback()
		panic(err)
	}

	//3 only if no err or panic, commit
	err = tx.Commit()
	if err != nil {
		panic(err)
	}
}

// Difference returns the set difference for util.StringSet.
func Difference(mainSet util.StringSet, getDiff util.StringSet) util.StringSet {
	ret := make([]string, 0, len(getDiff))
	for k := range getDiff {
		if !mainSet.Contains(k) {
			ret = append(ret, k)
		}
	}
	return util.NewStringSet(ret...)
}

