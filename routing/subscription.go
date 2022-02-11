// Copyright (c) 2021 Contributors to the Eclipse Foundation
//
// See the NOTICE file(s) distributed with this work for additional
// information regarding copyright ownership.
//
// This program and the accompanying materials are made available under the
// terms of the Eclipse Public License 2.0 which is available at
// http://www.eclipse.org/legal/epl-2.0
//
// SPDX-License-Identifier: EPL-2.0

package routing

import (
	"container/list"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/eclipse-kanto/suite-connector/connector"
	"github.com/eclipse-kanto/suite-connector/util"
)

const (
	// TopicLogSubscribe is topic for log messages about subscriptions.
	TopicLogSubscribe = "$SYS/broker/log/M/subscribe"
	// TopicLogUnsubscribe is topic for log messages about unsubscriptions.
	TopicLogUnsubscribe = "$SYS/broker/log/M/unsubscribe"
	// TopicLogNoticeLevel is topic for log messages with notice level.
	TopicLogNoticeLevel = "$SYS/broker/log/N"

	topicLog = "$SYS/broker/log/#"
)

var (
	subscribeRegexp = regexp.MustCompile(
		`(?P<Time>\d{6,11}): (?P<ClientId>[[:print:]]+) (?P<QoS>[0-2]{1}) (?P<Topic>(c|command)//[[:print:]]+/(q|req)/#)`)
	unsubscribeRegexp = regexp.MustCompile(
		`(?P<Time>\d{6,11}): (?P<ClientId>[[:print:]]+) (?P<Topic>(c|command)//[[:print:]]+/(q|req)/#)`)
	disconnectedRegexp = regexp.MustCompile(
		`(?P<Time>\d{6,11}): Client (?P<ClientId>[[:print:]]+) (?P<Topic>disconnected.)`)
	disconnectingRegexp = regexp.MustCompile(
		`(?P<Time>\d{6,11}): Socket error on client (?P<ClientId>[[:print:]]+), (?P<Topic>disconnecting.)`)
	closedConnectionRegexp = regexp.MustCompile(
		`(?P<Time>\d{6,11}): Client (?P<ClientId>[[:print:]]+) (?P<Topic>closed its connection.)`)
)

var (
	localClientID = "connector"
	cloudClientID = "cloud"
)

func init() {
	if localID := os.Getenv("LOCAL_CLIENTID"); len(localID) > 0 {
		localClientID = localID
	}

	if cloudID := os.Getenv("CLOUD_CLIENTID"); len(cloudID) > 0 {
		cloudClientID = cloudID
	}
}

type logTimestamp struct {
	time.Time
}

func (t logTimestamp) MarshalJSON() ([]byte, error) {
	stamp := fmt.Sprintf(`"%s"`, t.Format(time.RFC3339Nano))
	return []byte(stamp), nil
}

func parseLogTimestamp(s string) (logTimestamp, error) {
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return logTimestamp{time.Time{}}, err
	}
	return logTimestamp{time.Unix(sec, 0)}, nil
}

// SubscriptionItem represents a subscription data.
type SubscriptionItem struct {
	ClientID  string       `json:"clientId,omitempty"`
	Timestamp logTimestamp `json:"timestamp"`
	TopicID   string       `json:"topicId,omitempty"`
	TopicReal string       `json:"topicReal,omitempty"`
}

// String returns the SubscriptionItem string representation.
func (s SubscriptionItem) String() string {
	r, _ := json.Marshal(&s)
	return string(r)
}

// LogHandler processes Mosquitto log messages
type LogHandler struct {
	SubcriptionList *list.List
	Manager         connector.SubscriptionManager

	Logger watermill.LoggerAdapter
}

// CreateLogHandler creates handler that subscribes to Mosquitto logging topic
func CreateLogHandler(router *message.Router,
	manager connector.SubscriptionManager,
	localClient *connector.MQTTConnection,
) {
	h := &LogHandler{
		SubcriptionList: list.New(),
		Manager:         manager,
		Logger:          router.Logger(),
	}

	router.AddNoPublisherHandler("logs_bus",
		topicLog,
		connector.NewSubscriber(localClient, connector.QosAtMostOnce, true, router.Logger(), nil),
		h.ProcessLogs,
	)
}

// ProcessLogs is called once new log notification comes from Mosquitto.
// It evaluates the topic and message if relates to Local subscriptions and if so take the proper action
func (h *LogHandler) ProcessLogs(msg *message.Message) error {
	topic, ok := connector.TopicFromCtx(msg.Context())
	if !ok {
		return nil
	}

	h.Logger.Debug(fmt.Sprint("[mosquitto] ", topic, " ", string(msg.Payload)), nil)

	logMessage := ParseLogMessage(topic, string(msg.Payload))
	if logMessage == nil {
		return nil
	}

	// skip own subscriptions
	if logMessage.ClientID == localClientID {
		return nil
	}

	// skip own subscriptions
	if logMessage.ClientID == cloudClientID {
		return nil
	}

	// for all other clients evaluate the message
	switch logMessage.Type {
	case Subscription:
		h.processLogsSubscriptionType(logMessage)

	case UnSubscription:
		topicNorm := util.NormalizeTopic(logMessage.Text)
		removed := h.remove(topicNorm)
		if removed != nil {
			// forward unsubscriptions with the real topic
			h.Manager.Remove(removed.TopicReal)
			h.Logger.Info(fmt.Sprintf("Forwarded unsubscribe %v", removed), nil)
		}
	case Disconnection, Termination:
		// if client close or terminate remove all subscriptions for this client and forward all
		removedList := h.removeClient(logMessage.ClientID)
		for e := removedList.Front(); e != nil; e = e.Next() {
			i := e.Value.(SubscriptionItem)
			// forward unsubscription with the real topic
			h.Manager.Remove(i.TopicReal)
			h.Logger.Info(fmt.Sprintf("Forward unsubscribe %v due to %#v", i, logMessage.Type), nil)
		}

	default:
		// do nothing
	}

	return nil
}

func (h *LogHandler) processLogsSubscriptionType(logMessage *LogMessage) {
	topicNorm := util.NormalizeTopic(logMessage.Text)
	itemCandidate := SubscriptionItem{
		ClientID:  logMessage.ClientID,
		Timestamp: logMessage.Timestamp,
		TopicID:   topicNorm,
		TopicReal: logMessage.Text,
	}
	itemCurrent := h.get(topicNorm)
	if itemCurrent != nil {
		// exists
		if itemCurrent.TopicReal != logMessage.Text || itemCurrent.ClientID != logMessage.ClientID {
			// exists but with different naming or client, re-add it with the new one
			h.remove(topicNorm)
			h.add(itemCandidate)
			// forward subscription with the real topic
			h.Manager.Add(itemCandidate.TopicReal)
			msg := "Subscription with different format or client exist %v, replaced and forwarded the new %#v"
			h.Logger.Info(fmt.Sprintf(msg, itemCurrent, itemCandidate), nil)
		} else {
			h.Logger.Info(fmt.Sprintf("The same subscription exists %v, no action", itemCandidate), nil)
		}
	} else {
		// new subscription
		h.add(itemCandidate)
		// forward subscription with the real topic
		h.Manager.Add(itemCandidate.TopicReal)
		h.Logger.Info(fmt.Sprintf("Forwarded new subscription %v", itemCandidate), nil)
	}
}

func (h *LogHandler) add(item SubscriptionItem) {
	h.SubcriptionList.PushBack(item)
}

func (h *LogHandler) remove(TopicID string) *SubscriptionItem {
	for e := h.SubcriptionList.Front(); e != nil; e = e.Next() {
		i := e.Value.(SubscriptionItem)
		if i.TopicID == TopicID {
			h.SubcriptionList.Remove(e)
			return &i
		}
	}
	return nil
}

func (h *LogHandler) removeClient(clientID string) *list.List {
	removedList := list.New()

	var next *list.Element
	for e := h.SubcriptionList.Front(); e != nil; e = next {
		next = e.Next()
		i := e.Value.(SubscriptionItem)
		if i.ClientID == clientID {
			removedList.PushBack(h.SubcriptionList.Remove(e))
		}
	}

	return removedList
}

func (h *LogHandler) get(TopicID string) *SubscriptionItem {
	for e := h.SubcriptionList.Front(); e != nil; e = e.Next() {
		i := e.Value.(SubscriptionItem)
		if i.TopicID == TopicID {
			return &i
		}
	}
	return nil
}

type logMessageType int

const (
	// Subscription message type.
	Subscription logMessageType = iota
	// UnSubscription message type.
	UnSubscription
	// Disconnection message type.
	Disconnection
	// Termination message type.
	Termination
)

// LogMessage represents a subscription message data.
type LogMessage struct {
	ClientID  string
	Timestamp logTimestamp
	Text      string
	Type      logMessageType
}

// ParseLogMessage parse log message to logMessage structure
func ParseLogMessage(topic, message string) *LogMessage {
	switch topic {
	case TopicLogSubscribe:
		if match := subscribeRegexp.FindStringSubmatch(message); match != nil {
			if timestamp, err := parseLogTimestamp(match[1]); err == nil {
				return &LogMessage{
					ClientID:  match[2],
					Timestamp: timestamp,
					Text:      match[4],
					Type:      Subscription,
				}
			}
		}

	case TopicLogUnsubscribe:
		if match := unsubscribeRegexp.FindStringSubmatch(message); match != nil {
			if timestamp, err := parseLogTimestamp(match[1]); err == nil {
				return &LogMessage{
					ClientID:  match[2],
					Timestamp: timestamp,
					Text:      match[3],
					Type:      UnSubscription,
				}
			}
		}

	case TopicLogNoticeLevel:
		return parseLogMessageNoticeLevel(topic, message)

	default:
		// do nothing
	}
	return nil
}

func parseLogMessageNoticeLevel(topic, message string) *LogMessage {
	if match := disconnectedRegexp.FindStringSubmatch(message); match != nil {
		if timestamp, err := parseLogTimestamp(match[1]); err == nil {
			return &LogMessage{
				ClientID:  match[2],
				Timestamp: timestamp,
				Text:      match[3],
				Type:      Disconnection,
			}
		}
	}

	if match := disconnectingRegexp.FindStringSubmatch(message); match != nil {
		if timestamp, err := parseLogTimestamp(match[1]); err == nil {
			return &LogMessage{
				ClientID:  match[2],
				Timestamp: timestamp,
				Text:      match[3],
				Type:      Termination,
			}
		}
	}

	if match := closedConnectionRegexp.FindStringSubmatch(message); match != nil {
		if timestamp, err := parseLogTimestamp(match[1]); err == nil {
			return &LogMessage{
				ClientID:  match[2],
				Timestamp: timestamp,
				Text:      match[3],
				Type:      Termination,
			}
		}
	}
	return nil
}
