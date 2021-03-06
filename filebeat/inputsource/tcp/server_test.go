// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package tcp

import (
	"bufio"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/stretchr/testify/assert"

	"github.com/elastic/beats/filebeat/inputsource"
	"github.com/elastic/beats/libbeat/common"
)

var defaultConfig = Config{
	Timeout:        time.Minute * 5,
	MaxMessageSize: 20 * humanize.MiByte,
}

type info struct {
	message string
	mt      inputsource.NetworkMetadata
}

func TestErrorOnEmptyLineDelimiter(t *testing.T) {
	c := common.NewConfig()
	config := defaultConfig
	err := c.Unpack(&config)
	assert.Error(t, err)
}

func TestReceiveEventsAndMetadata(t *testing.T) {
	expectedMessages := generateMessages(5, 100)
	largeMessages := generateMessages(10, 4096)

	tests := []struct {
		name             string
		cfg              map[string]interface{}
		splitFunc        bufio.SplitFunc
		expectedMessages []string
		messageSent      string
	}{
		{
			name:             "NewLine",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte("\n")),
			expectedMessages: expectedMessages,
			messageSent:      strings.Join(expectedMessages, "\n"),
		},
		{
			name:             "NewLineWithCR",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte("\r\n")),
			expectedMessages: expectedMessages,
			messageSent:      strings.Join(expectedMessages, "\r\n"),
		},
		{
			name:             "CustomDelimiter",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte(";")),
			expectedMessages: expectedMessages,
			messageSent:      strings.Join(expectedMessages, ";"),
		},
		{
			name:             "MultipleCharsCustomDelimiter",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte("<END>")),
			expectedMessages: expectedMessages,
			messageSent:      strings.Join(expectedMessages, "<END>"),
		},
		{
			name:             "SingleCharCustomDelimiterMessageWithoutBoundaries",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte(";")),
			expectedMessages: []string{"hello"},
			messageSent:      "hello",
		},
		{
			name:             "MultipleCharCustomDelimiterMessageWithoutBoundaries",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte("<END>")),
			expectedMessages: []string{"hello"},
			messageSent:      "hello",
		},
		{
			name:             "NewLineMessageWithoutBoundaries",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte("\n")),
			expectedMessages: []string{"hello"},
			messageSent:      "hello",
		},
		{
			name:             "NewLineLargeMessagePayload",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte("\n")),
			expectedMessages: largeMessages,
			messageSent:      strings.Join(largeMessages, "\n"),
		},
		{
			name:             "CustomLargeMessagePayload",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte(";")),
			expectedMessages: largeMessages,
			messageSent:      strings.Join(largeMessages, ";"),
		},
		{
			name:             "MaxReadBufferReached",
			cfg:              map[string]interface{}{},
			splitFunc:        SplitFunc([]byte("\n")),
			expectedMessages: []string{},
			messageSent:      randomString(900000),
		},
		{
			name:      "MaxReadBufferReachedUserConfigured",
			splitFunc: SplitFunc([]byte("\n")),
			cfg: map[string]interface{}{
				"max_read_message": 50000,
			},
			expectedMessages: []string{},
			messageSent:      randomString(600000),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ch := make(chan *info, len(test.expectedMessages))
			defer close(ch)
			to := func(message []byte, mt inputsource.NetworkMetadata) {
				ch <- &info{message: string(message), mt: mt}
			}
			test.cfg["host"] = "localhost:0"
			cfg, _ := common.NewConfigFrom(test.cfg)
			config := defaultConfig
			err := cfg.Unpack(&config)
			if !assert.NoError(t, err) {
				return
			}
			server, err := New(&config, test.splitFunc, to)
			if !assert.NoError(t, err) {
				return
			}
			err = server.Start()
			if !assert.NoError(t, err) {
				return
			}
			defer server.Stop()

			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			assert.NoError(t, err)
			fmt.Fprint(conn, test.messageSent)
			conn.Close()

			var events []*info

			for len(events) < len(test.expectedMessages) {
				select {
				case event := <-ch:
					events = append(events, event)
				default:
				}
			}

			for idx, e := range events {
				assert.Equal(t, test.expectedMessages[idx], e.message)
				assert.NotNil(t, e.mt.RemoteAddr)
			}
		})
	}
}

func TestReceiveNewEventsConcurrently(t *testing.T) {
	workers := 4
	eventsCount := 100
	ch := make(chan *info, eventsCount*workers)
	defer close(ch)
	to := func(message []byte, mt inputsource.NetworkMetadata) {
		ch <- &info{message: string(message), mt: mt}
	}
	cfg, err := common.NewConfigFrom(map[string]interface{}{"host": ":0"})
	if !assert.NoError(t, err) {
		return
	}
	config := defaultConfig
	err = cfg.Unpack(&config)
	if !assert.NoError(t, err) {
		return
	}
	server, err := New(&config, bufio.ScanLines, to)
	if !assert.NoError(t, err) {
		return
	}
	err = server.Start()
	if !assert.NoError(t, err) {
		return
	}
	defer server.Stop()

	samples := generateMessages(eventsCount, 1024)
	for w := 0; w < workers; w++ {
		go func() {
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			defer conn.Close()
			assert.NoError(t, err)
			for _, sample := range samples {
				fmt.Fprintln(conn, sample)
			}
		}()
	}

	var events []*info
	for len(events) < eventsCount*workers {
		select {
		case event := <-ch:
			events = append(events, event)
		default:
		}
	}
}

func randomString(l int) string {
	charsets := []byte("abcdefghijklmnopqrstuvwzyzABCDEFGHIJKLMNOPQRSTUVWZYZ0123456789")
	message := make([]byte, l)
	for i := range message {
		message[i] = charsets[rand.Intn(len(charsets))]
	}
	return string(message)
}

func generateMessages(c int, l int) []string {
	messages := make([]string, c)
	for i := range messages {
		messages[i] = randomString(l)
	}
	return messages
}
