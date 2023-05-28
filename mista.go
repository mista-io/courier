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
	"fmt"

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

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(_ context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	

	

	apiKey := "Bearer" + " " + msg.Channel().StringConfigForKey(courier.ConfigAPIKey, "")
	if apiKey == "" {
		return nil, fmt.Errorf("no API key set for Mista channel")
	}

	type RequestParams struct {
		REC           string   `json:"recipient"`
		SENDER        string   `json:"sender_id"`
		MSG           string   `json:"message"`
		TYPE          string   `json:"type"`
		
	}

	// build our request
	form := RequestParams{
		
		REC:       msg.URN().Path(),
		SENDER:       msg.Channel().Address(),
		MSG:  handlers.GetTextAndAttachments(msg),
		TYPE   :  "plain",
	

	}
    
    var body io.Reader

	marshalled, err := json.Marshal(form)
	if err != nil {
		return nil, err
	}
	body = bytes.NewReader(marshalled)
	sendMethod := http.MethodPost

	req, err := http.NewRequest(sendMethod, sendURL, body)
	if err != nil {
		return nil, err
	}	
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", apiKey)

	rr, err := utils.MakeHTTPRequest(req)

	// record our status and log
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	status.AddLog(courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err))
	if err != nil {
		return status, nil
	}

	// was this request successful?
	msgStatus, _ := jsonparser.GetString([]byte(rr.Body), "data", "status")
	if msgStatus != "Delivered" {
		status.SetStatus(courier.MsgErrored)
		return status, nil
	}

	// grab the external id if we can
	externalID, _ := jsonparser.GetString([]byte(rr.Body), "data", "uid")
	status.SetStatus(courier.MsgWired)
	status.SetExternalID(externalID)

	return status, nil
}
