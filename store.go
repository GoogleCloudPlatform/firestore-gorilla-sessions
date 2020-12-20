// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package firestoregorilla is a Firestore-backed sessions store, which can be
// used with gorilla/sessions.
//
// Encoded sessions are stored in Firestore and session IDs are stored in
// secure cookies.
//
// Sessions never expire and are never deleted or cleaned up.
package firestoregorilla

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"cloud.google.com/go/firestore"
	"github.com/gorilla/sessions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxLength is the maximum length of an encoded session that can be stored
// in a Store. See https://firebase.google.com/docs/firestore/quotas.
const maxLength = 2 << 20

// Store is a Firestore-backed sessions store.
type Store struct {
	client *firestore.Client
}

var _ sessions.Store = &Store{}

// sessionDoc wraps an encoded session so it can be saved as a Firestore
// document.
type sessionDoc struct {
	EncodedSession string
}

// New creates a new Store.
//
// Only string key values are supported for sessions.
func New(ctx context.Context, client *firestore.Client) (*Store, error) {
	return &Store{
		client: client,
	}, nil
}

// Get returns a cached session, if it exists. Otherwise, Get returns a new
// session.
//
// The name is used as the cookie name and Firestore collection name, so
// different apps in the same Google Cloud project should use different names.
func (s *Store) Get(r *http.Request, name string) (*sessions.Session, error) {
	return sessions.GetRegistry(r).Get(s, name)
}

// New creates and returns a new session.
//
// If the session already exists, it will be returned.
//
// The name is used as the cookie name and Firestore collection name, so
// different apps in the same Google Cloud project should use different names.
func (s *Store) New(r *http.Request, name string) (*sessions.Session, error) {
	session := sessions.NewSession(s, name)

	// Ignore errors in case the cookie isn't set yet.
	id, _ := s.readIDFromCookie(r, name)
	if id == "" {
		// No ID in the cookie means the session is new.
		session.IsNew = true
		return session, nil
	}

	// ID found, check if the session already exists.
	ds, err := s.client.Collection(name).Doc(id).Get(r.Context())
	if status.Code(err) == codes.NotFound {
		// A NotFound error means the session is new.
		session.IsNew = true
		return session, nil
	}
	if err != nil {
		return session, fmt.Errorf("Get: %v", err)
	}

	// The session was found, get it.
	encoded := sessionDoc{}
	if err := ds.DataTo(&encoded); err != nil {
		return session, fmt.Errorf("DataTo: %v", err)
	}
	cachedSession, err := s.deserialize(encoded.EncodedSession)
	if err != nil {
		return session, err
	}
	session.ID = cachedSession.ID
	session.Values = cachedSession.Values
	session.Options = cachedSession.Options
	session.IsNew = false

	return session, nil
}

// Save persists the session to Firestore and the session ID in a cookie.
func (s *Store) Save(r *http.Request, w http.ResponseWriter, session *sessions.Session) error {
	id := session.ID
	if id == "" {
		// Ignore errors in case the cookie isn't set yet.
		id, _ = s.readIDFromCookie(r, session.Name())
	}
	if id == "" {
		id = s.client.Collection(session.Name()).NewDoc().ID
	}

	session.ID = id
	sessionString, err := s.serialize(session)
	if err != nil {
		return err
	}
	encoded := sessionDoc{EncodedSession: sessionString}

	if _, err := s.client.Collection(session.Name()).Doc(id).Set(r.Context(), encoded); err != nil {
		return fmt.Errorf("Create: %v", err)
	}

	s.saveIDInCookie(w, session.Name(), id, session.Options)

	return nil
}

// readIDFromCookie gets the ID stored int the cookie, if it exists.
// It returns an error if the cookie is not set.
func (s *Store) readIDFromCookie(r *http.Request, name string) (string, error) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", fmt.Errorf("Cookie: %v", err)
	}
	return c.Value, nil
}

// saveIDInCookie saves the ID and session options in a cookie.
func (s *Store) saveIDInCookie(w http.ResponseWriter, name, id string, opts *sessions.Options) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    id,
		Path:     opts.Path,
		Domain:   opts.Domain,
		MaxAge:   opts.MaxAge,
		Secure:   opts.Secure,
		HttpOnly: opts.HttpOnly,
		SameSite: opts.SameSite,
	}
	http.SetCookie(w, cookie)
}

// jsonSession is an encoding/json compatible version of sessions.Session.
type jsonSession struct {
	Values  map[string]interface{}
	ID      string
	Options *sessions.Options
}

// serialize serializes the session into a JSON string. Only string key values
// are supported. encoding/gob could be used to support non-string keys, but it
// is slower and leads to larger sessions.
func (s *Store) serialize(session *sessions.Session) (string, error) {
	values := map[string]interface{}{}
	for k, v := range session.Values {
		ks, ok := k.(string)
		if !ok {
			return "", fmt.Errorf("only string keys supported: %v", k)
		}
		values[ks] = v
	}
	jSession := jsonSession{
		Values:  values,
		ID:      session.ID,
		Options: session.Options,
	}
	b, err := json.Marshal(jSession)
	if err != nil {
		return "", fmt.Errorf("json.Marshal: %v", err)
	}
	if len(b) > maxLength {
		return "", fmt.Errorf("max length of session exceeded: %d > %d", len(b), maxLength)
	}
	return string(b), nil
}

// deserialize decodes a session.
func (*Store) deserialize(s string) (*sessions.Session, error) {
	jSession := jsonSession{}
	if err := json.Unmarshal([]byte(s), &jSession); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %v", err)
	}
	values := map[interface{}]interface{}{}
	for k, v := range jSession.Values {
		values[k] = v
	}
	return &sessions.Session{
		Values:  values,
		ID:      jSession.ID,
		Options: jSession.Options,
	}, nil
}
