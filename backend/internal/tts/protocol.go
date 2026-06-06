// Package tts 实现火山引擎「大模型语音合成 双向流式 V3」的 WebSocket 二进制
// 协议编解码与客户端。
//
// 帧结构(整数均为大端):
//
//	frame = header(4B) + event(4B) + [sessionID_size(4B) + sessionID] + payload_size(4B) + payload
//
// header 4 字节(本项目固定用「带事件」的全量客户端请求):
//
//	byte0: 高4位=协议版本(0b0001),低4位=header 大小(单位4字节,0b0001 => 4B)
//	byte1: 高4位=message type,低4位=flags(0b0100 = 带 event 号)
//	byte2: 高4位=序列化(JSON=0b0001),低4位=压缩(none=0b0000)
//	byte3: 0x00
//
// 会话事件(StartSession/TaskRequest/FinishSession)携带 sessionID;
// 连接事件(StartConnection/FinishConnection)不携带。
package tts

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 协议常量。
const (
	protocolVersion = 0b0001
	defaultHeaderSz = 0b0001 // 实际 header 字节数 = 该值 * 4

	// message type(byte1 高 4 位)
	msgFullClientReq  = 0b0001 // 端上:全量客户端请求
	msgFullServerResp = 0b1001 // 服务端:全量响应(事件/JSON)
	msgAudioOnlyResp  = 0b1011 // 服务端:音频数据响应
	msgErrorResp      = 0b1111 // 服务端:错误

	// flags(byte1 低 4 位)
	flagWithEvent = 0b0100 // 帧中带 4 字节 event 号

	// 序列化(byte2 高 4 位)
	serialNone = 0b0000
	serialJSON = 0b0001

	// 压缩(byte2 低 4 位)
	compNone = 0b0000
	compGzip = 0b0001
)

// 事件号(event)。
const (
	EventStartConnection   int32 = 1
	EventFinishConnection  int32 = 2
	EventConnectionStarted int32 = 50
	EventConnectionFailed  int32 = 51
	EventStartSession      int32 = 100
	EventFinishSession     int32 = 102
	EventSessionStarted    int32 = 150
	EventSessionFinished   int32 = 152
	EventSessionFailed     int32 = 153
	EventTaskRequest       int32 = 200
	EventTTSSentenceStart  int32 = 350
	EventTTSSentenceEnd    int32 = 351
	EventTTSResponse       int32 = 352
)

// ErrShortFrame 表示帧数据长度不足。
var ErrShortFrame = errors.New("tts: 帧长度不足")

func gzipDecompress(in []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// buildClientFrame 构造一帧「带 event 的全量客户端请求」。
// sessionID 为空表示连接级事件(不写 sessionID 字段)。
func buildClientFrame(event int32, sessionID string, payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte((protocolVersion << 4) | defaultHeaderSz)
	buf.WriteByte((msgFullClientReq << 4) | flagWithEvent)
	buf.WriteByte((serialJSON << 4) | compNone)
	buf.WriteByte(0x00)

	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(event))
	buf.Write(u32[:])

	if sessionID != "" {
		binary.BigEndian.PutUint32(u32[:], uint32(len(sessionID)))
		buf.Write(u32[:])
		buf.WriteString(sessionID)
	}

	binary.BigEndian.PutUint32(u32[:], uint32(len(payload)))
	buf.Write(u32[:])
	buf.Write(payload)
	return buf.Bytes()
}

// ServerFrame 是解析后的服务端帧。
type ServerFrame struct {
	MessageType byte
	Event       int32
	SessionID   string
	Payload     []byte // JSON 或音频字节(去框、必要时解压后)
	IsError     bool
	ErrorCode   uint32
}

// parseServerFrame 解析一帧服务端二进制消息。
//
// 解析策略:先读 header 与 event;错误帧单独处理;连接级事件(50/51/52)只关心
// event 本身,不再解析 sessionID/payload(避免连接帧是否含 id 的歧义);其余
// 会话/音频事件统一按 [sessionID(len+bytes)] + [payload(len+bytes)] 解析。
func parseServerFrame(data []byte) (*ServerFrame, error) {
	if len(data) < 4 {
		return nil, ErrShortFrame
	}
	b0, b1, b2 := data[0], data[1], data[2]
	headerSz := int(b0&0x0f) * 4
	if headerSz < 4 {
		headerSz = 4
	}
	if len(data) < headerSz {
		return nil, ErrShortFrame
	}
	msgType := b1 >> 4
	flags := b1 & 0x0f
	comp := b2 & 0x0f
	idx := headerSz

	f := &ServerFrame{MessageType: msgType}

	readU32 := func() (uint32, error) {
		if len(data) < idx+4 {
			return 0, ErrShortFrame
		}
		v := binary.BigEndian.Uint32(data[idx : idx+4])
		idx += 4
		return v, nil
	}
	readBytes := func(n int) ([]byte, error) {
		if n < 0 || len(data) < idx+n {
			return nil, ErrShortFrame
		}
		b := data[idx : idx+n]
		idx += n
		return b, nil
	}

	if msgType == msgErrorResp {
		f.IsError = true
		if flags&flagWithEvent != 0 {
			ev, err := readU32()
			if err != nil {
				return nil, err
			}
			f.Event = int32(ev)
		}
		code, err := readU32()
		if err != nil {
			return nil, err
		}
		f.ErrorCode = code
		size, err := readU32()
		if err != nil {
			return nil, err
		}
		payload, err := readBytes(int(size))
		if err != nil {
			return nil, err
		}
		f.Payload = maybeGunzip(payload, comp)
		return f, nil
	}

	if flags&flagWithEvent != 0 {
		ev, err := readU32()
		if err != nil {
			return nil, err
		}
		f.Event = int32(ev)
	}

	// 连接级事件:只关心 event,不再解析后续字段。
	if f.Event == EventStartConnection || f.Event == EventFinishConnection ||
		f.Event == EventConnectionStarted || f.Event == EventConnectionFailed {
		return f, nil
	}

	// 会话/音频事件:sessionID(len+bytes) + payload(len+bytes)。
	if sz, err := readU32(); err == nil {
		if sid, err := readBytes(int(sz)); err == nil {
			f.SessionID = string(sid)
		} else {
			return nil, err
		}
	} else {
		return nil, err
	}
	size, err := readU32()
	if err != nil {
		return nil, err
	}
	payload, err := readBytes(int(size))
	if err != nil {
		return nil, err
	}
	f.Payload = maybeGunzip(payload, comp)
	return f, nil
}

// maybeGunzip 在压缩位为 gzip 时解压,否则原样返回(解压失败也回退原样)。
func maybeGunzip(payload []byte, comp byte) []byte {
	if comp != compGzip {
		return payload
	}
	if dec, err := gzipDecompress(payload); err == nil {
		return dec
	}
	return payload
}

// describeEvent 给日志用的事件名。
func describeEvent(event int32) string {
	switch event {
	case EventConnectionStarted:
		return "ConnectionStarted"
	case EventConnectionFailed:
		return "ConnectionFailed"
	case EventSessionStarted:
		return "SessionStarted"
	case EventSessionFinished:
		return "SessionFinished"
	case EventSessionFailed:
		return "SessionFailed"
	case EventTTSSentenceStart:
		return "TTSSentenceStart"
	case EventTTSSentenceEnd:
		return "TTSSentenceEnd"
	case EventTTSResponse:
		return "TTSResponse"
	default:
		return fmt.Sprintf("Event(%d)", event)
	}
}
