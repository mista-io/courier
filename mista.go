package mista

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
)

var sendURL = "https://api.mista.io/sms"

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("MX"), "Mista")}
}

type moForm struct {
	ID   string `name:"id"`
	Body string `validate:"required" name:"body"`
	From string `validate:"required" name:"from"`
	To   string `validate:"required" name:"to"`
	Date string `name:"date"`
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "receive", h.receiveMessage)
	s.AddHandlerRoute(h, http.MethodPost, "status", h.receiveStatus)
	return nil
}

// receiveMessage is our HTTP handler function for incoming messages
func (h *handler) receiveMessage(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	// get our params
	form := &moForm{}
	err := handlers.DecodeAndValidateForm(form, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	fmt.Printf("Received date: %s\n", form.Date) // Print the received date for debugging purposes

	// Parse the date string
	var date time.Time
	if form.Date != "" {
		parsedTime, err := time.Parse(time.RFC3339, form.Date)
		if err != nil {
			parsedTime, err = time.Parse("2006-01-02T15:04:05Z", form.Date)
			if err != nil {
				return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, fmt.Errorf("invalid date format: %s", form.Date))
			}
		}
		// Convert to UTC
		date = parsedTime.UTC()
	}

	// create our URN
	urn, err := handlers.StrictTelForCountry(form.From, channel.Country())
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// build our msg
	msg := h.Backend().NewIncomingMsg(channel, urn, form.Body).WithExternalID(form.ID).WithReceivedOn(date)

	// and finally write our message
	return handlers.WriteMsgsAndResponse(ctx, h, []courier.Msg{msg}, w, r)
}

type statusForm struct {
	ID     string `validate:"required" name:"id"`
	Status string `validate:"required" name:"status"`
}

var statusMapping = map[string]courier.MsgStatusValue{
	"Success":  courier.MsgDelivered,
	"Sent":     courier.MsgSent,
	"Buffered": courier.MsgSent,
	"Rejected": courier.MsgFailed,
	"Failed":   courier.MsgFailed,
	"Expired":  courier.MsgFailed,
}

// receiveStatus is our HTTP handler function for status updates
func (h *handler) receiveStatus(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	// get our params
	form := &statusForm{}
	err := handlers.DecodeAndValidateForm(form, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	msgStatus, found := statusMapping[form.Status]
	if !found {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r,
			fmt.Errorf("unknown status '%s', must be one of 'Success','Sent','Buffered','Rejected', 'Failed', or 'Expired'", form.Status))
	}

	// write our status
	status := h.Backend().NewMsgStatusForExternalID(channel, form.ID, msgStatus)
	return handlers.WriteMsgStatusAndResponse(ctx, h, channel, status, w, r)
}

// SendMsg sends the passed-in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	apiKey := "Bearer " + msg.Channel().StringConfigForKey(courier.ConfigAPIKey, "")
	if apiKey == "" {
		return nil, fmt.Errorf("no API key set for Mista channel")
	}

	type RequestParams struct {
		Recipient string `json:"recipient"`
		SenderID  string `json:"sender_id"`
		Message   string `json:"message"`
		Type      string `json:"type"`
	}

	// Build our request
	form := RequestParams{
		Recipient: msg.URN().Path(),
		SenderID:  msg.Channel().Address(),
		Message:   msg.Text(),
		Type:      "plain",
	}

	marshalled, err := json.Marshal(form)
	if err != nil {
		return nil, err
	}

	body := bytes.NewReader(marshalled)

	req, err := http.NewRequest(http.MethodPost, sendURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", apiKey)

	client := http.DefaultClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resp != nil {
			resp.Body.Close()
		}
	}()

	// Check if the response is nil
	if resp == nil {
		return nil, errors.New("nil response received")
	}

	// Read the response body
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Check the response status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SMS request failed with status code: %d", resp.StatusCode)
	}

	// record our status and log the error
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	status.AddLog(courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), nil).WithError("Message Send Error", err))

	// Parse the response body to extract the necessary information
	var responseData struct {
		Status string `json:"status"`
		UID    string `json:"uid"`
	}

	err = json.Unmarshal(respBody, &responseData)
	if err != nil {
		return status, nil
	}

	// Create the message status based on the response data
	status.SetStatus(courier.MsgWired)
	status.SetExternalID(responseData.UID)

	return status, nil
}
