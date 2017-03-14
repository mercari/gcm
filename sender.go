// Google Cloud Messaging for application servers implemented using the
// Go programming language.
package gcm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"time"
)

const (
	// FCMSendEndpoint is the endpoint for sending message to the Firebase Cloud Messaging (FCM).
	// See more on https://firebase.google.com/docs/cloud-messaging/server
	FCMSendEndpoint = "https://fcm.googleapis.com/fcm/senda"

	// GcmSendEndpoint is the endpoint for sending messages to the GCM server.
	// Firebase Cloud Messaging (FCM) is the new version of GCM. Should use new endpoint.
	// See more on https://firebase.google.com/support/faq/#gcm-fcm
	GcmSendEndpoint = "https://gcm-http.googleapis.com/gcm/send"
)

const (
	// Initial delay before first retry, without jitter.
	backoffInitialDelay = 1000

	// Maximum delay before a retry.
	maxBackoffDelay = 1024000

	// maxRegistrationIDs are max number of registration IDs in one message.
	maxRegistrationIDs = 1000

	// maxTimeToLive is max time GCM storage can store messages when the device is offline
	maxTimeToLive = 2419200 // 4 weeks
)

// Declared as a mutable variable for testing purposes.
// Use GCMEndpoint by default for backward compatibility.
var defaultEndpoint = GcmSendEndpoint

// Sender abstracts the interaction between the application server and the
// GCM server. The developer must obtain an API key from the Google APIs
// Console page and pass it to the Sender so that it can perform authorized
// requests on the application server's behalf. To send a message to one or
// more devices use the Sender's Send or SendNoRetry methods.
//
// If the Http field is nil, a zeroed http.Client will be allocated and used
// to send messages. If your application server runs on Google AppEngine,
// you must use the "appengine/urlfetch" package to create the *http.Client
// as follows:
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//		c := appengine.NewContext(r)
//		client := urlfetch.Client(c)
//		sender := &gcm.Sender{ApiKey: key, Http: client}
//
//		/* ... */
//	}
type Sender struct {
	ApiKey string
	URL    string
	Http   *http.Client
}

// NewClient returns a new sender with the given URL and apiKey.
// If one of input is empty or URL is malformed, returns error.
// It sets http.DefaultHTTP client for http connection to server.
// If you need our own configuration overwrite it.
func NewClient(urlString, apiKey string) (*Sender, error) {
	if len(urlString) == 0 {
		return nil, fmt.Errorf("missing GCM/FCM endpoint url")
	}

	if len(apiKey) == 0 {
		return nil, fmt.Errorf("missing API Key")
	}

	if _, err := url.Parse(urlString); err != nil {
		return nil, fmt.Errorf("failed to parse URL %q: %s", urlString, err)
	}

	return &Sender{
		URL:    urlString,
		ApiKey: apiKey,
		Http:   http.DefaultClient,
	}, nil
}

// SendNoRetry sends a message to the GCM server without retrying in case of
// service unavailability. A non-nil error is returned if a non-recoverable
// error occurs (i.e. if the response status is not "200 OK").
func (s *Sender) SendNoRetry(msg *Message) (*Response, error) {
	if err := checkSender(s); err != nil {
		return nil, err
	} else if err := checkMessage(msg); err != nil {
		return nil, err
	}

	return s.send(msg)
}

// Send sends a message to the GCM server, retrying in case of service
// unavailability. A non-nil error is returned if a non-recoverable
// error occurs (i.e. if the response status is not "200 OK").
//
// Note that messages are retried using exponential backoff, and as a
// result, this method may block for several seconds.
func (s *Sender) Send(msg *Message, retries int) (*Response, error) {
	if err := checkSender(s); err != nil {
		return nil, err
	} else if err := checkMessage(msg); err != nil {
		return nil, err
	} else if retries < 0 {
		return nil, errors.New("'retries' must not be negative.")
	}

	// Send the message for the first time.
	resp, err := s.send(msg)
	if err != nil {
		return nil, err
	} else if resp.Failure == 0 || retries == 0 {
		return resp, nil
	}

	// One or more messages failed to send.
	regIDs := msg.RegistrationIDs
	allResults := make(map[string]Result, len(regIDs))
	backoff := backoffInitialDelay
	for i := 0; updateStatus(msg, resp, allResults) > 0 && i < retries; i++ {
		sleepTime := backoff/2 + rand.Intn(backoff)
		time.Sleep(time.Duration(sleepTime) * time.Millisecond)
		backoff = min(2*backoff, maxBackoffDelay)
		if resp, err = s.send(msg); err != nil {
			msg.RegistrationIDs = regIDs
			return nil, err
		}
	}

	// Bring the message back to its original state.
	msg.RegistrationIDs = regIDs

	// Create a Response containing the overall results.
	finalResults := make([]Result, len(regIDs))
	var success, failure, canonicalIDs int
	for i := 0; i < len(regIDs); i++ {
		result, _ := allResults[regIDs[i]]
		finalResults[i] = result
		if result.MessageID != "" {
			if result.RegistrationID != "" {
				canonicalIDs++
			}
			success++
		} else {
			failure++
		}
	}

	return &Response{
		// Return the most recent multicast id.
		MulticastID:  resp.MulticastID,
		Success:      success,
		Failure:      failure,
		CanonicalIDs: canonicalIDs,
		Results:      finalResults,
	}, nil
}

func (s *Sender) send(msg *Message) (*Response, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	if err := encoder.Encode(msg); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", s.URL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("key=%s", s.ApiKey))
	req.Header.Add("Content-Type", "application/json")

	resp, err := s.Http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid status code %d: %s", resp.StatusCode, resp.Status)
	}

	var response Response
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&response); err != nil {
		return nil, err
	}

	return &response, err
}

// updateStatus updates the status of the messages sent to devices and
// returns the number of recoverable errors that could be retried.
func updateStatus(msg *Message, resp *Response, allResults map[string]Result) int {
	unsentRegIDs := make([]string, 0, resp.Failure)
	for i := 0; i < len(resp.Results); i++ {
		regID := msg.RegistrationIDs[i]
		allResults[regID] = resp.Results[i]
		if resp.Results[i].Error == "Unavailable" {
			unsentRegIDs = append(unsentRegIDs, regID)
		}
	}
	msg.RegistrationIDs = unsentRegIDs
	return len(unsentRegIDs)
}

// min returns the smaller of two integers. For exciting religious wars
// about why this wasn't included in the "math" package, see this thread:
// https://groups.google.com/d/topic/golang-nuts/dbyqx_LGUxM/discussion
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// checkSender returns an error if the sender is not well-formed and
// initializes a zeroed http.Client if one has not been provided.
func checkSender(sender *Sender) error {
	if sender.ApiKey == "" {
		return errors.New("the sender's API key must not be empty")
	}

	if sender.Http == nil {
		sender.Http = new(http.Client)
	}

	// Previously, by default, this library uses gcm endpoint.
	// To keep backwards compatibility, use GCM endpoint when not specified.
	if sender.URL == "" {
		sender.URL = defaultEndpoint
	}
	return nil
}

// checkMessage returns an error if the message is not well-formed.
func checkMessage(msg *Message) error {
	if msg == nil {
		return errors.New("the message must not be nil")
	} else if msg.RegistrationIDs == nil {
		return errors.New("the message's RegistrationIDs field must not be nil")
	} else if len(msg.RegistrationIDs) == 0 {
		return errors.New("the message must specify at least one registration ID")
	} else if len(msg.RegistrationIDs) > maxRegistrationIDs {
		return errors.New("the message may specify at most 1000 registration IDs")
	} else if msg.TimeToLive < 0 || maxTimeToLive < msg.TimeToLive {
		return errors.New("the message's TimeToLive field must be an integer " +
			"between 0 and 2419200 (4 weeks)")
	}
	return nil
}
