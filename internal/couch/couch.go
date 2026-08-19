// Package couch is the slice of CouchDB's HTTP API this service needs.
//
// Two databases are involved and they are used very differently. The library (`notes`) is read
// only, and only its page documents — this service never writes there, because the ink and the
// notebooks belong to the tablets' sync protocol and nothing here is entitled to touch them.
// The text database (`notes_text`) is read and written, one `pagetext:` document per page.
package couch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one CouchDB server.
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client
}

func New(baseURL, username, password string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Username: username,
		Password: password,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

// ErrNotFound is a 404, which for most callers here is information rather than failure: a page
// with no recognized text yet is the normal starting state.
var ErrNotFound = fmt.Errorf("not found")

type statusError struct {
	Status int
	Path   string
	Body   string
}

func (e *statusError) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200]
	}
	return fmt.Sprintf("%s returned %d: %s", e.Path, e.Status, body)
}

// Status returns the HTTP status behind err, or 0 when it was not a status error.
func Status(err error) int {
	var se *statusError
	if ok := asStatusError(err, &se); ok {
		return se.Status
	}
	return 0
}

func asStatusError(err error, target **statusError) bool {
	for err != nil {
		if se, ok := err.(*statusError); ok {
			*target = se
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Username != "" {
		credentials := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
		req.Header.Set("Authorization", "Basic "+credentials)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, &statusError{Status: resp.StatusCode, Path: path, Body: string(payload)}
	}
	return payload, nil
}

// Ping checks that a database exists and these credentials can read it.
func (c *Client) Ping(ctx context.Context, database string) error {
	_, err := c.do(ctx, http.MethodGet, "/"+url.PathEscape(database), nil, nil)
	return err
}

// Get reads one document into out.
func (c *Client) Get(ctx context.Context, database, id string, out any) error {
	payload, err := c.do(ctx, http.MethodGet, "/"+url.PathEscape(database)+"/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, out)
}

// Put writes one document, returning the HTTP status so a caller can tell a conflict from a
// failure. A 409 is not an error here: it is another writer having got there first, which the
// publishing rules have an answer for.
func (c *Client) Put(ctx context.Context, database, id string, doc any) (int, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}
	_, err = c.do(ctx, http.MethodPut, "/"+url.PathEscape(database)+"/"+url.PathEscape(id), nil, body)
	if err != nil {
		if status := Status(err); status != 0 {
			return status, nil
		}
		return 0, err
	}
	return http.StatusCreated, nil
}

// Change is one row of the change feed.
type Change struct {
	ID      string
	Seq     string
	Deleted bool
}

// Changes reads a batch of changes since the given sequence.
//
// Documents are deliberately not included. The feed is used to find out *which* pages moved; the
// page documents themselves are megabytes of stroke data, and fetching them for every change
// would mean downloading the entire library to discover that most of it needs nothing done.
func (c *Client) Changes(ctx context.Context, database, since string, limit int) (string, []Change, error) {
	query := url.Values{}
	query.Set("since", since)
	query.Set("limit", strconv.Itoa(limit))

	payload, err := c.do(ctx, http.MethodGet, "/"+url.PathEscape(database)+"/_changes", query, nil)
	if err != nil {
		return since, nil, err
	}

	var body struct {
		LastSeq json.RawMessage `json:"last_seq"`
		Results []struct {
			ID      string          `json:"id"`
			Seq     json.RawMessage `json:"seq"`
			Deleted bool            `json:"deleted"`
		} `json:"results"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return since, nil, err
	}

	changes := make([]Change, 0, len(body.Results))
	for _, row := range body.Results {
		changes = append(changes, Change{
			ID:      row.ID,
			Seq:     rawToString(row.Seq),
			Deleted: row.Deleted,
		})
	}
	last := rawToString(body.LastSeq)
	if last == "" {
		last = since
	}
	return last, changes, nil
}

// rawToString unwraps a sequence, which CouchDB gives as a string in a cluster and as a number
// on a single node. Keeping it as text either way means the checkpoint round-trips unchanged.
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}
