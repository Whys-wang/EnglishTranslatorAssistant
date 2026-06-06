package tts

import (
	"encoding/binary"
	"testing"
)

// 构造一帧服务端二进制消息(测试辅助)。
func makeServerFrame(msgType, flags byte, event int32, sessionID string, payload []byte, withEvent, withSession bool) []byte {
	var b []byte
	b = append(b, (protocolVersion<<4)|defaultHeaderSz, (msgType<<4)|flags, (serialJSON<<4)|compNone, 0x00)
	u := func(v uint32) {
		var t [4]byte
		binary.BigEndian.PutUint32(t[:], v)
		b = append(b, t[:]...)
	}
	if withEvent {
		u(uint32(event))
	}
	if withSession {
		u(uint32(len(sessionID)))
		b = append(b, sessionID...)
	}
	u(uint32(len(payload)))
	b = append(b, payload...)
	return b
}

func TestBuildClientFrame_ConnectionEvent(t *testing.T) {
	frame := buildClientFrame(EventStartConnection, "", []byte("{}"))
	// header(4) + event(4) + payloadSize(4) + payload(2) = 14
	if len(frame) != 14 {
		t.Fatalf("帧长度 = %d, want 14", len(frame))
	}
	if frame[0] != 0x11 || frame[1] != 0x14 || frame[2] != 0x10 || frame[3] != 0x00 {
		t.Fatalf("header = % x, want 11 14 10 00", frame[:4])
	}
	if ev := int32(binary.BigEndian.Uint32(frame[4:8])); ev != EventStartConnection {
		t.Fatalf("event = %d, want %d", ev, EventStartConnection)
	}
	if sz := binary.BigEndian.Uint32(frame[8:12]); sz != 2 {
		t.Fatalf("payload size = %d, want 2", sz)
	}
	if string(frame[12:]) != "{}" {
		t.Fatalf("payload = %q, want {}", string(frame[12:]))
	}
}

func TestBuildClientFrame_SessionEvent(t *testing.T) {
	sid := "sess-abc"
	frame := buildClientFrame(EventTaskRequest, sid, []byte("hi"))
	idx := 4
	if ev := int32(binary.BigEndian.Uint32(frame[idx : idx+4])); ev != EventTaskRequest {
		t.Fatalf("event = %d, want %d", ev, EventTaskRequest)
	}
	idx += 4
	sz := int(binary.BigEndian.Uint32(frame[idx : idx+4]))
	idx += 4
	if sz != len(sid) || string(frame[idx:idx+sz]) != sid {
		t.Fatalf("sessionID 字段不符: size=%d got=%q", sz, string(frame[idx:idx+sz]))
	}
	idx += sz
	psz := int(binary.BigEndian.Uint32(frame[idx : idx+4]))
	idx += 4
	if psz != 2 || string(frame[idx:idx+psz]) != "hi" {
		t.Fatalf("payload 字段不符: size=%d got=%q", psz, string(frame[idx:idx+psz]))
	}
}

func TestParseServerFrame_ConnectionStarted(t *testing.T) {
	data := makeServerFrame(msgFullServerResp, flagWithEvent, EventConnectionStarted, "", nil, true, false)
	f, err := parseServerFrame(data)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if f.Event != EventConnectionStarted {
		t.Fatalf("event = %d, want %d", f.Event, EventConnectionStarted)
	}
	if f.IsError {
		t.Fatal("不应是错误帧")
	}
}

func TestParseServerFrame_Audio(t *testing.T) {
	sid := "sess-xyz"
	audio := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	data := makeServerFrame(msgAudioOnlyResp, flagWithEvent, EventTTSResponse, sid, audio, true, true)
	f, err := parseServerFrame(data)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if f.Event != EventTTSResponse {
		t.Fatalf("event = %d, want %d", f.Event, EventTTSResponse)
	}
	if f.SessionID != sid {
		t.Fatalf("sessionID = %q, want %q", f.SessionID, sid)
	}
	if string(f.Payload) != string(audio) {
		t.Fatalf("audio payload = % x, want % x", f.Payload, audio)
	}
}

func TestParseServerFrame_SessionFinished(t *testing.T) {
	sid := "s1"
	data := makeServerFrame(msgFullServerResp, flagWithEvent, EventSessionFinished, sid, []byte(`{"code":20000000}`), true, true)
	f, err := parseServerFrame(data)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if f.Event != EventSessionFinished {
		t.Fatalf("event = %d, want %d", f.Event, EventSessionFinished)
	}
}

func TestParseServerFrame_Error(t *testing.T) {
	var b []byte
	b = append(b, (protocolVersion<<4)|defaultHeaderSz, (msgErrorResp<<4)|0x00, (serialJSON<<4)|compNone, 0x00)
	var code [4]byte
	binary.BigEndian.PutUint32(code[:], 45000001)
	b = append(b, code[:]...)
	msg := []byte("bad request")
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(msg)))
	b = append(b, sz[:]...)
	b = append(b, msg...)

	f, err := parseServerFrame(b)
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if !f.IsError {
		t.Fatal("应识别为错误帧")
	}
	if f.ErrorCode != 45000001 {
		t.Fatalf("errorCode = %d, want 45000001", f.ErrorCode)
	}
	if string(f.Payload) != "bad request" {
		t.Fatalf("payload = %q", string(f.Payload))
	}
}

func TestParseServerFrame_Short(t *testing.T) {
	if _, err := parseServerFrame([]byte{0x11, 0x94}); err == nil {
		t.Fatal("应对过短帧报错")
	}
}
