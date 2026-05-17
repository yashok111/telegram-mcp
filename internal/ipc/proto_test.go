package ipc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestRequestMarshal(t *testing.T) {
	r := Request{Jsonrpc: "2.0", ID: 7, Method: "bot.sendMessage", Params: json.RawMessage(`{"chat_id":"1"}`)}
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":7,"method":"bot.sendMessage","params":{"chat_id":"1"}}`, string(b))
}

func TestResponseUnmarshalResult(t *testing.T) {
	var r Response
	require.NoError(t, json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":3,"result":{"message_id":42}}`), &r))
	assert.Equal(t, uint64(3), r.ID)
	assert.Nil(t, r.Error)
	assert.JSONEq(t, `{"message_id":42}`, string(r.Result))
}

func TestResponseUnmarshalError(t *testing.T) {
	var r Response
	require.NoError(t, json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32001,"message":"not allowlisted","data":{"chat_id":"123"}}}`), &r))
	require.NotNil(t, r.Error)
	assert.Equal(t, CodeNotAllowlisted, r.Error.Code)
	assert.Equal(t, "not allowlisted", r.Error.Message)
	assert.JSONEq(t, `{"chat_id":"123"}`, string(r.Error.Data))
}

func TestNotificationHasNoID(t *testing.T) {
	n := Notification{Jsonrpc: "2.0", Method: "notifications/inbound", Params: json.RawMessage(`{"content":"hi"}`)}
	b, err := json.Marshal(n)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"id"`)
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
