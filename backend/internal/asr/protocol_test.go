package asr

import (
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestGzipRoundTrip(t *testing.T) {
	orig := []byte("火山引擎 ASR 二进制协议 gzip round-trip 测试 12345")
	gz, err := gzipCompress(orig)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	got, err := gzipDecompress(gz)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(got) != string(orig) {
		t.Fatalf("round-trip mismatch: %q != %q", got, orig)
	}
}

func TestBuildFullClientRequestHeader(t *testing.T) {
	frame, err := buildFullClientRequest([]byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(frame) < 8 {
		t.Fatalf("frame too short: %d", len(frame))
	}
	if got := frame[0] >> 4; got != protocolVersion {
		t.Errorf("version = %d, want %d", got, protocolVersion)
	}
	if got := frame[0] & 0x0f; got != defaultHeaderSz {
		t.Errorf("header size = %d, want %d", got, defaultHeaderSz)
	}
	if got := frame[1] >> 4; got != msgFullClientReq {
		t.Errorf("msg type = %d, want %d", got, msgFullClientReq)
	}
	if got := frame[1] & 0x0f; got != flagNoSeq {
		t.Errorf("flags = %d, want %d", got, flagNoSeq)
	}
	if got := frame[2] >> 4; got != serialJSON {
		t.Errorf("serialization = %d, want %d", got, serialJSON)
	}
	if got := frame[2] & 0x0f; got != compGzip {
		t.Errorf("compression = %d, want %d", got, compGzip)
	}
	// payload size 字段应与其后字节数一致。
	size := binary.BigEndian.Uint32(frame[4:8])
	if int(size) != len(frame)-8 {
		t.Errorf("payload size = %d, want %d", size, len(frame)-8)
	}
}

func TestBuildAudioRequestLastFlag(t *testing.T) {
	normal, _ := buildAudioRequest([]byte{1, 2, 3}, false)
	last, _ := buildAudioRequest([]byte{1, 2, 3}, true)
	if got := normal[1] & 0x0f; got != flagNoSeq {
		t.Errorf("normal flags = %d, want %d", got, flagNoSeq)
	}
	if got := last[1] & 0x0f; got != flagLastNoSeq {
		t.Errorf("last flags = %d, want %d", got, flagLastNoSeq)
	}
	if got := normal[1] >> 4; got != msgAudioOnlyReq {
		t.Errorf("audio msg type = %d, want %d", got, msgAudioOnlyReq)
	}
	if got := normal[2] >> 4; got != serialNone {
		t.Errorf("audio serialization = %d, want %d", got, serialNone)
	}
}

// buildTestServerFrame 模拟服务端构造一个 full server response 帧(可选带 sequence、gzip)。
func buildTestServerFrame(t *testing.T, payload []byte, withSeq bool, seq int32, last bool, gzipPayload bool) []byte {
	t.Helper()
	flags := byte(flagNoSeq)
	switch {
	case withSeq && last:
		flags = flagNegSeq
	case withSeq:
		flags = flagPosSeq
	case last:
		flags = flagLastNoSeq
	}
	comp := byte(compNone)
	if gzipPayload {
		comp = compGzip
		gz, err := gzipCompress(payload)
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		payload = gz
	}
	h := buildHeader(msgFullServerResp, flags, serialJSON, comp)
	out := append([]byte{}, h[:]...)
	if withSeq {
		var sb [4]byte
		binary.BigEndian.PutUint32(sb[:], uint32(seq))
		out = append(out, sb[:]...)
	}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, payload...)
	return out
}

func TestParseServerFrameWithSequenceAndGzip(t *testing.T) {
	jsonPayload := []byte(`{"result":{"text":"hello world","utterances":[{"text":"hello world","start_time":0,"end_time":1200,"definite":true}]}}`)
	frame := buildTestServerFrame(t, jsonPayload, true, 7, true, true)

	got, err := parseServerFrame(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.MessageType != msgFullServerResp {
		t.Errorf("msg type = %d", got.MessageType)
	}
	if !got.HasSequence || got.Sequence != 7 {
		t.Errorf("sequence = %d hasSeq=%v, want 7/true", got.Sequence, got.HasSequence)
	}
	if !got.IsLast {
		t.Errorf("IsLast = false, want true")
	}
	var p serverPayload
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.Result.Text != "hello world" {
		t.Errorf("text = %q", p.Result.Text)
	}
	if len(p.Result.Utterances) != 1 || !p.Result.Utterances[0].Definite {
		t.Errorf("utterances parsed wrong: %+v", p.Result.Utterances)
	}
}

func TestParseServerFrameNoSeqNoGzip(t *testing.T) {
	jsonPayload := []byte(`{"result":{"text":"测试","utterances":[]}}`)
	frame := buildTestServerFrame(t, jsonPayload, false, 0, false, false)
	got, err := parseServerFrame(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.HasSequence {
		t.Errorf("should not have sequence")
	}
	if got.IsLast {
		t.Errorf("should not be last")
	}
	if string(got.Payload) != string(jsonPayload) {
		t.Errorf("payload = %q", got.Payload)
	}
}

func TestParseErrorFrame(t *testing.T) {
	msg := []byte("invalid request")
	h := buildHeader(msgErrorResp, flagNoSeq, serialJSON, compNone)
	out := append([]byte{}, h[:]...)
	var code [4]byte
	binary.BigEndian.PutUint32(code[:], 45000001)
	out = append(out, code[:]...)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(msg)))
	out = append(out, sz[:]...)
	out = append(out, msg...)

	got, err := parseServerFrame(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.MessageType != msgErrorResp {
		t.Errorf("msg type = %d", got.MessageType)
	}
	if got.ErrorCode != 45000001 {
		t.Errorf("error code = %d", got.ErrorCode)
	}
	if string(got.Payload) != string(msg) {
		t.Errorf("error msg = %q", got.Payload)
	}
}

func TestParseShortFrame(t *testing.T) {
	if _, err := parseServerFrame([]byte{0x11, 0x90}); err == nil {
		t.Errorf("expected error for short frame")
	}
}
