// Package asr 实现火山引擎「大模型流式语音识别」(BigModel SAUC)的
// WebSocket 二进制协议编解码与客户端。
//
// 协议要点(全部整数大端):
//
//	frame = header(4B) + [可选 sequence(4B)] + payload size(4B) + payload
//
// header 4 字节:
//
//	byte0: 高4位 = 协议版本(0b0001),低4位 = header 大小(单位:4字节,通常 0b0001 => 4B)
//	byte1: 高4位 = message type,低4位 = message type specific flags
//	byte2: 高4位 = 序列化方式(JSON=0b0001 / none=0b0000),低4位 = 压缩(gzip=0b0001 / none=0b0000)
//	byte3: 保留(0x00)
package asr

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
	msgFullClientReq  = 0b0001 // 端上:含请求参数的 full client request
	msgAudioOnlyReq   = 0b0010 // 端上:含音频数据的 audio only request
	msgFullServerResp = 0b1001 // 服务端:含识别结果的 full server response
	msgErrorResp      = 0b1111 // 服务端:错误消息

	// message type specific flags(byte1 低 4 位)
	flagNoSeq     = 0b0000 // header 后无 sequence
	flagPosSeq    = 0b0001 // header 后有 sequence(正)
	flagLastNoSeq = 0b0010 // 无 sequence,且为最后一包(负包)
	flagNegSeq    = 0b0011 // header 后有 sequence(负,最后一包)

	// 序列化(byte2 高 4 位)
	serialNone = 0b0000
	serialJSON = 0b0001

	// 压缩(byte2 低 4 位)
	compNone = 0b0000
	compGzip = 0b0001
)

// SuccessCode 是火山返回的成功错误码。
const SuccessCode = 20000000

// gzipCompress 用 gzip 压缩数据。
func gzipCompress(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(in); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gzipDecompress 解压 gzip 数据。
func gzipDecompress(in []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// buildHeader 组装 4 字节 header。
func buildHeader(msgType, flags, serial, comp byte) [4]byte {
	return [4]byte{
		(protocolVersion << 4) | defaultHeaderSz,
		(msgType << 4) | flags,
		(serial << 4) | comp,
		0x00,
	}
}

// buildClientFrame 构造端上发送的帧(full client request / audio only request)。
// 端上帧不带 sequence 字段:header + payload size + payload。
func buildClientFrame(msgType, flags, serial, comp byte, payload []byte) []byte {
	h := buildHeader(msgType, flags, serial, comp)
	out := make([]byte, 0, 4+4+len(payload))
	out = append(out, h[:]...)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, payload...)
	return out
}

// buildFullClientRequest 构造首包:JSON 配置,gzip 压缩。
func buildFullClientRequest(jsonPayload []byte) ([]byte, error) {
	gz, err := gzipCompress(jsonPayload)
	if err != nil {
		return nil, fmt.Errorf("gzip config: %w", err)
	}
	return buildClientFrame(msgFullClientReq, flagNoSeq, serialJSON, compGzip, gz), nil
}

// buildAudioRequest 构造音频包:原始 PCM,gzip 压缩。last 为 true 表示最后一包(负包)。
func buildAudioRequest(pcm []byte, last bool) ([]byte, error) {
	gz, err := gzipCompress(pcm)
	if err != nil {
		return nil, fmt.Errorf("gzip audio: %w", err)
	}
	flags := byte(flagNoSeq)
	if last {
		flags = flagLastNoSeq
	}
	return buildClientFrame(msgAudioOnlyReq, flags, serialNone, compGzip, gz), nil
}

// ServerFrame 是解析后的服务端帧。
type ServerFrame struct {
	MessageType byte
	Flags       byte
	IsLast      bool   // 是否最后一包(flags 含「负包」位)
	Sequence    int32  // 当存在 sequence 时有效
	HasSequence bool   // 是否携带 sequence
	Payload     []byte // 解压并去框后的负载(JSON 或错误信息)
	ErrorCode   uint32 // 仅 msgErrorResp 时有效
}

// ErrShortFrame 表示帧数据长度不足。
var ErrShortFrame = errors.New("asr: 帧长度不足")

// parseServerFrame 解析服务端二进制帧。
func parseServerFrame(data []byte) (*ServerFrame, error) {
	if len(data) < 4 {
		return nil, ErrShortFrame
	}
	b0, b1, b2 := data[0], data[1], data[2]

	headerSz := int(b0&0x0f) * 4
	if headerSz < 4 {
		headerSz = 4
	}
	msgType := b1 >> 4
	flags := b1 & 0x0f
	comp := b2 & 0x0f

	if len(data) < headerSz {
		return nil, ErrShortFrame
	}
	idx := headerSz

	f := &ServerFrame{
		MessageType: msgType,
		Flags:       flags,
		IsLast:      flags == flagLastNoSeq || flags == flagNegSeq,
		HasSequence: flags == flagPosSeq || flags == flagNegSeq,
	}

	readU32 := func() (uint32, error) {
		if len(data) < idx+4 {
			return 0, ErrShortFrame
		}
		v := binary.BigEndian.Uint32(data[idx : idx+4])
		idx += 4
		return v, nil
	}

	switch msgType {
	case msgFullServerResp:
		if f.HasSequence {
			seq, err := readU32()
			if err != nil {
				return nil, err
			}
			f.Sequence = int32(seq)
		}
		size, err := readU32()
		if err != nil {
			return nil, err
		}
		if len(data) < idx+int(size) {
			return nil, ErrShortFrame
		}
		payload := data[idx : idx+int(size)]
		if comp == compGzip {
			payload, err = gzipDecompress(payload)
			if err != nil {
				return nil, fmt.Errorf("gunzip payload: %w", err)
			}
		}
		f.Payload = payload
		return f, nil

	case msgErrorResp:
		code, err := readU32()
		if err != nil {
			return nil, err
		}
		f.ErrorCode = code
		size, err := readU32()
		if err != nil {
			return nil, err
		}
		if len(data) < idx+int(size) {
			return nil, ErrShortFrame
		}
		payload := data[idx : idx+int(size)]
		if comp == compGzip {
			if dec, derr := gzipDecompress(payload); derr == nil {
				payload = dec
			}
		}
		f.Payload = payload
		return f, nil

	default:
		// 其它类型(理论上端到端不会收到),原样返回剩余字节。
		f.Payload = data[idx:]
		return f, nil
	}
}
