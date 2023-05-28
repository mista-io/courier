package mista

import (
	"context"
	"fmt"
	"net/http"
	// "net/url"
	// "strings"
	"io"
	"encoding/json"
	"time"
	"bytes"

	"github.com/buger/jsonparser"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
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
	ID   string `json:validate:"required" name:"id"`
	Text string `json:validate:"required" name:"text"`
	From string `json:validate:"required" name:"from"`
	To   string `json:validate:"required" name:"to"`
	Date string `json:validate:"required" name:"date"`
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "receive", h.receiveMessage)
	s.AddHandlerRoute(h, http.MethodPost, "callback", h.receiveMessage)
	s.AddHandlerRoute(h, http.MethodPost, "delivery", h.receiveStatus)
	s.AddHandlerRoute(h, http.MethodPost, "status", h.receiveStatus)
	return nil
}

// receiveMessage is our HTTP handler function for incoming messages
func (h *handler) receiveMessage(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	// get our params
	fmt.Println("Reached#####")
	form := &moForm{}
	err := handlers.DecodeAndValidateForm(form, r)
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}

	// create our date from the timestamp
	// 2017-05-03T06:04:45Z
	date, err := time.Parse("2006-01-02T15:04:05Z", form.Date)
	if err != nil {
		date, err = time.Parse("2006-01-02 15:04:05", form.Date)
		if err != nil {
			return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, fmt.Errorf("invalid date format: %s", form.Date))
		}
		date = date.UTC()
	}

	// create our URN
	urn, err := handlers.StrictTelForCountry(form.From, channel.Country())
	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}
	// build our msg
	msg := h.Backend().NewIncomingMsg(channel, urn, form.Text).WithExternalID(form.ID).WithReceivedOn(date)

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
		Recipient: msg.Channel().Address(),
		SenderID:  msg.Channel().StringConfigForKey(courier.ConfigSenderID, ""),
		Message:   msg.Text(),
		Type:      "plain",
	}

	marshalled, err := json.Marshal(form)
	if err != nil {
		return nil, err
	}

	body := bytes.NewReader(marshalled)
	sendURL := "https://api.mista.io/sms" // Set the correct URL for sending SMS

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
	defer resp.Body.Close()

	// Read the response body
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Check the response status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SMS request failed with status code: %d", resp.StatusCode)
	}

	// Parse the response body to extract the necessary information
	var responseData struct {
		Status string `json:"status"`
		UID    string `json:"uid"`
	}

	err = json.Unmarshal(respBody, &responseData)
	if err != nil {
		return nil, err
	}

	// Create the message status based on the response data
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	if responseData.Status == "Delivered" {
		status.SetStatus(courier.MsgWired)
		status.SetExternalID(responseData.UID)
	} else {
		status.SetStatus(courier.MsgErrored)
	}

	return status, nil
}

