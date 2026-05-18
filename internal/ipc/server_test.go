package ipc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeResponseMatchesStructMarshal(t *testing.T) {
	cases := []struct {
		name   string
		id     uint64
		result any
		rpcErr *Error
	}{
		{name: "result_nil", id: 1},
		{name: "result_scalar", id: 2, result: 42},
		{name: "result_map", id: 3, result: map[string]any{"message_id": 99, "ok": true}},
		{
			name: "result_nested",
			id:   4,
			result: map[string]any{
				"peers": []any{
					map[string]any{"alias": "@s1", "idle_seconds": 0, "meta": map[string]any{"label": "shimA"}},
					map[string]any{"alias": "@s2", "idle_seconds": 12, "meta": map[string]any{"label": "shimB"}},
				},
				"count": 2,
			},
		},
		{name: "result_slice", id: 5, result: []int{1, 2, 3}},
		{name: "result_unicode", id: 6, result: map[string]string{"text": "héllo ☃ \"world\""}},
		{name: "error_only", id: 7, rpcErr: &Error{Code: CodeBotError, Message: "boom"}},
		{
			name:   "error_with_data",
			id:     8,
			rpcErr: &Error{Code: CodeNotAllowlisted, Message: "deny", Data: json.RawMessage(`{"chat_id":"123"}`)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				raw json.RawMessage
				err error
			)

			if tc.rpcErr == nil && tc.result != nil {
				raw, err = json.Marshal(tc.result)
				require.NoError(t, err)
			}

			got, err := encodeResponse(tc.id, raw, tc.rpcErr)
			require.NoError(t, err)

			ref := Response{Jsonrpc: JSONRPCVersion, ID: tc.id, Result: raw, Error: tc.rpcErr}
			want, err := json.Marshal(ref)
			require.NoError(t, err)

			assert.JSONEq(t, string(want), string(got))

			var rt Response
			require.NoError(t, json.Unmarshal(got, &rt))
			assert.Equal(t, tc.id, rt.ID)

			switch {
			case tc.rpcErr != nil:
				require.NotNil(t, rt.Error)
				assert.Equal(t, tc.rpcErr.Code, rt.Error.Code)
				assert.Equal(t, tc.rpcErr.Message, rt.Error.Message)
			case tc.result != nil:
				assert.JSONEq(t, string(raw), string(rt.Result))
			}
		})
	}
}

func TestEncodeNotificationMatchesStructMarshal(t *testing.T) {
	cases := []struct {
		name   string
		method string
		params any
	}{
		{name: "no_params", method: "ping"},
		{name: "simple_params", method: "notifications/inbound", params: map[string]string{"content": "hi"}},
		{
			name:   "nested_params",
			method: NotifyPermissionResolved,
			params: map[string]any{"request_id": "r1", "approved": true, "meta": map[string]int{"n": 7}},
		},
		{name: "method_with_special_chars", method: "weird/method.name", params: map[string]int{"x": 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := marshalParams(tc.params)
			require.NoError(t, err)

			got, err := encodeNotification(tc.method, raw)
			require.NoError(t, err)

			ref := Notification{Jsonrpc: JSONRPCVersion, Method: tc.method, Params: raw}
			want, err := json.Marshal(ref)
			require.NoError(t, err)

			assert.JSONEq(t, string(want), string(got))

			assert.NotContains(t, string(got), `"id"`)
		})
	}
}

func TestEncodeRequestMatchesStructMarshal(t *testing.T) {
	cases := []struct {
		name   string
		id     uint64
		method string
		params any
	}{
		{name: "no_params", id: 1, method: "ping"},
		{name: "simple", id: 2, method: "bot.sendMessage", params: map[string]string{"chat_id": "1", "text": "hi"}},
		{
			name:   "nested",
			id:     999,
			method: MethodBotBroadcastPermissionRequest,
			params: map[string]any{"request_id": "abc", "options": []string{"yes", "no"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := marshalParams(tc.params)
			require.NoError(t, err)

			got, err := encodeRequest(tc.id, tc.method, raw)
			require.NoError(t, err)

			ref := Request{Jsonrpc: JSONRPCVersion, ID: tc.id, Method: tc.method, Params: raw}
			want, err := json.Marshal(ref)
			require.NoError(t, err)

			assert.JSONEq(t, string(want), string(got))
		})
	}
}

func TestServer_responseRoundTrip_preservesResult(t *testing.T) {
	s, sock := startTestServer(t)

	type nested struct {
		Name string         `json:"name"`
		Tags []string       `json:"tags"`
		Meta map[string]int `json:"meta"`
	}

	payload := map[string]any{
		"id":      int64(42),
		"items":   []nested{{Name: "a", Tags: []string{"x", "y"}, Meta: map[string]int{"k": 1}}, {Name: "b", Tags: []string{}, Meta: map[string]int{"k": 2, "j": 3}}},
		"count":   2,
		"empty":   []any{},
		"unicode": "héllo ☃",
	}

	s.Handle("complex", func(_ context.Context, _ *Conn, _ json.RawMessage) (any, *Error) {
		return payload, nil
	})

	c, err := Dial(sock)
	require.NoError(t, err)

	defer c.Close()

	var got map[string]any

	require.NoError(t, c.Call(t.Context(), "complex", nil, &got))

	wantJSON, err := json.Marshal(payload)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)

	assert.JSONEq(t, string(wantJSON), string(gotJSON))
}

func TestServer_methodNotFound_returnsError(t *testing.T) {
	_, sock := startTestServer(t)

	c, err := Dial(sock)
	require.NoError(t, err)

	defer c.Close()

	err = c.Call(t.Context(), "nonexistent", nil, nil)
	require.Error(t, err)

	var rpcErr *Error
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, CodeMethodNotFound, rpcErr.Code)
}
