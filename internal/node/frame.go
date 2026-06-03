package node

import "encoding/json"

// Frame is one ndjson line on the pump<->client wire. Logged conversation frames carry Seq>0
// (user/agent/thought/tool/turn); transient frames carry Seq==0 (perm_request, reset); client->pump
// frames are prompt / perm_response. A single struct (sparse) keeps the codec trivial; Kind selects.
type Frame struct {
	Seq     int64  `json:"seq,omitempty"`
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`   // user/agent/thought/prompt
	ToolID  string `json:"toolId,omitempty"` // tool
	Title   string `json:"title,omitempty"`  // tool / perm_request
	Status  string `json:"status,omitempty"` // tool
	State   string `json:"state,omitempty"`  // turn: busy|idle
	Queued  int    `json:"queued,omitempty"` // turn
	ReqID   string `json:"reqId,omitempty"`  // perm_request / perm_response
	Allow   bool   `json:"allow,omitempty"`  // perm_response
	FromSeq int64  `json:"fromSeq,omitempty"` // reset
}

func encodeFrame(f Frame) []byte {
	b, _ := json.Marshal(f)
	return append(b, '\n')
}

func decodeFrame(line []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, err
	}
	return f, nil
}
