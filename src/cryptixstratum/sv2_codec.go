package cryptixstratum

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/cryptix-network/cryptix-stratum-bridge-v3/src/gostratum"
)

const (
	sv2ExtStandard uint16 = 0x0000
	sv2ExtChannel  uint16 = 0x8000

	sv2MsgSetupConnection           uint8 = 0x00
	sv2MsgSetupConnectionSuccess    uint8 = 0x01
	sv2MsgSetupConnectionError      uint8 = 0x02
	sv2MsgOpenStandardMiningChannel uint8 = 0x10
	sv2MsgOpenMiningChannelSuccess  uint8 = 0x11
	sv2MsgOpenMiningChannelError    uint8 = 0x12
	sv2MsgNewMiningJob              uint8 = 0x15
	sv2MsgSubmitSharesStandard      uint8 = 0x1A
	sv2MsgSubmitSharesSuccess       uint8 = 0x1C
	sv2MsgSubmitSharesError         uint8 = 0x1D
	sv2MsgSetNewPrevHash            uint8 = 0x20
	sv2MsgSetTarget                 uint8 = 0x21
	sv2MaxFramePayloadBytes               = 1_048_576
)

type sv2Frame struct {
	ExtensionType uint16
	MessageType   uint8
	Payload       []byte
}

func readSV2Frame(reader io.Reader) (*sv2Frame, error) {
	header := make([]byte, 6)
	if _, err := io.ReadFull(reader, header); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err
	}

	extensionType := binary.LittleEndian.Uint16(header[0:2])
	messageType := header[2]
	payloadLen := int(header[3]) | int(header[4])<<8 | int(header[5])<<16
	if payloadLen < 0 || payloadLen > sv2MaxFramePayloadBytes {
		return nil, fmt.Errorf("sv2 frame too large: %d", payloadLen)
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(reader, payload); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, err
	}

	return &sv2Frame{
		ExtensionType: extensionType,
		MessageType:   messageType,
		Payload:       payload,
	}, nil
}

func encodeSV2Frame(extensionType uint16, messageType uint8, payload []byte) ([]byte, error) {
	if len(payload) > 0xFF_FFFF {
		return nil, fmt.Errorf("sv2 payload too large")
	}
	out := make([]byte, 0, 6+len(payload))
	out = binary.LittleEndian.AppendUint16(out, extensionType)
	out = append(out, messageType)
	length := len(payload)
	out = append(out, byte(length), byte(length>>8), byte(length>>16))
	out = append(out, payload...)
	return out, nil
}

func writeSV2Frame(ctx *gostratum.StratumContext, extensionType uint16, messageType uint8, payload []byte) error {
	frame, err := encodeSV2Frame(extensionType, messageType, payload)
	if err != nil {
		return err
	}
	return ctx.WriteRaw(frame)
}

func putU16(out *[]byte, value uint16) {
	*out = binary.LittleEndian.AppendUint16(*out, value)
}

func putU32(out *[]byte, value uint32) {
	*out = binary.LittleEndian.AppendUint32(*out, value)
}

func putStr0_255(out *[]byte, value string) {
	sanitized := make([]byte, 0, len(value))
	for i := 0; i < len(value) && len(sanitized) < 255; i++ {
		c := value[i]
		if c >= 32 && c <= 126 {
			sanitized = append(sanitized, c)
		}
	}
	*out = append(*out, byte(len(sanitized)))
	*out = append(*out, sanitized...)
}

type sv2Cursor struct {
	data []byte
	pos  int
}

func newSV2Cursor(data []byte) *sv2Cursor {
	return &sv2Cursor{data: data}
}

func (c *sv2Cursor) remaining() int {
	return len(c.data) - c.pos
}

func (c *sv2Cursor) readBytes(length int) ([]byte, bool) {
	if c.remaining() < length {
		return nil, false
	}
	out := c.data[c.pos : c.pos+length]
	c.pos += length
	return out, true
}

func (c *sv2Cursor) readU8() (uint8, bool) {
	value, ok := c.readBytes(1)
	if !ok {
		return 0, false
	}
	return value[0], true
}

func (c *sv2Cursor) readU16() (uint16, bool) {
	value, ok := c.readBytes(2)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint16(value), true
}

func (c *sv2Cursor) readU32() (uint32, bool) {
	value, ok := c.readBytes(4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(value), true
}

func (c *sv2Cursor) readStr0_255() (string, bool) {
	length, ok := c.readU8()
	if !ok {
		return "", false
	}
	value, ok := c.readBytes(int(length))
	if !ok {
		return "", false
	}
	return string(value), true
}

func parseSV2SetupConnectionPayload(payload []byte) (string, error) {
	cursor := newSV2Cursor(payload)
	if _, ok := cursor.readU8(); !ok { // protocol
		return "", fmt.Errorf("missing protocol")
	}
	if _, ok := cursor.readU16(); !ok { // min version
		return "", fmt.Errorf("missing min version")
	}
	if _, ok := cursor.readU16(); !ok { // max version
		return "", fmt.Errorf("missing max version")
	}
	if _, ok := cursor.readU32(); !ok { // flags
		return "", fmt.Errorf("missing flags")
	}
	if _, ok := cursor.readStr0_255(); !ok { // host
		return "", fmt.Errorf("missing host")
	}
	if _, ok := cursor.readU16(); !ok { // port
		return "", fmt.Errorf("missing port")
	}
	vendor, ok := cursor.readStr0_255()
	if !ok {
		return "", fmt.Errorf("missing vendor")
	}
	if _, ok := cursor.readStr0_255(); !ok { // hardware version
		return "", fmt.Errorf("missing hardware version")
	}
	firmware, ok := cursor.readStr0_255()
	if !ok {
		return "", fmt.Errorf("missing firmware")
	}
	if _, ok := cursor.readStr0_255(); !ok { // device id
		return "", fmt.Errorf("missing device id")
	}

	if firmware != "" {
		return firmware, nil
	}
	if vendor != "" {
		return vendor, nil
	}
	return "sv2-client", nil
}

func parseSV2OpenStandardChannelPayload(payload []byte) (uint32, string, error) {
	cursor := newSV2Cursor(payload)
	requestID, ok := cursor.readU32()
	if !ok {
		return 0, "", fmt.Errorf("missing request id")
	}
	worker, ok := cursor.readStr0_255()
	if !ok {
		return 0, "", fmt.Errorf("missing worker")
	}
	if _, ok := cursor.readBytes(4); !ok { // nominal hashrate f32
		return 0, "", fmt.Errorf("missing nominal hashrate")
	}
	if _, ok := cursor.readBytes(32); !ok { // max target
		return 0, "", fmt.Errorf("missing max target")
	}
	return requestID, worker, nil
}

func parseSV2SubmitSharePayload(payload []byte) (channelID, sequence, jobID, nonce, ntime uint32, err error) {
	cursor := newSV2Cursor(payload)
	channelID, ok := cursor.readU32()
	if !ok {
		return 0, 0, 0, 0, 0, fmt.Errorf("missing channel id")
	}
	sequence, ok = cursor.readU32()
	if !ok {
		return 0, 0, 0, 0, 0, fmt.Errorf("missing sequence")
	}
	jobID, ok = cursor.readU32()
	if !ok {
		return 0, 0, 0, 0, 0, fmt.Errorf("missing job id")
	}
	nonce, ok = cursor.readU32()
	if !ok {
		return 0, 0, 0, 0, 0, fmt.Errorf("missing nonce")
	}
	ntime, ok = cursor.readU32()
	if !ok {
		return 0, 0, 0, 0, 0, fmt.Errorf("missing ntime")
	}
	if _, ok := cursor.readU32(); !ok { // version
		return 0, 0, 0, 0, 0, fmt.Errorf("missing version")
	}
	return channelID, sequence, jobID, nonce, ntime, nil
}

func encodeSV2SetupConnectionSuccessPayload(usedVersion uint16, flags uint32) []byte {
	out := make([]byte, 0, 6)
	putU16(&out, usedVersion)
	putU32(&out, flags)
	return out
}

func encodeSV2SetupConnectionErrorPayload(flags uint32, code string) []byte {
	out := make([]byte, 0, 64)
	putU32(&out, flags)
	putStr0_255(&out, code)
	return out
}

func encodeSV2OpenChannelSuccessPayload(requestID, channelID uint32, targetLE [32]byte, extranonce []byte) []byte {
	if len(extranonce) > 255 {
		extranonce = extranonce[:255]
	}
	out := make([]byte, 0, 4+4+32+1+len(extranonce)+4)
	putU32(&out, requestID)
	putU32(&out, channelID)
	out = append(out, targetLE[:]...)
	out = append(out, byte(len(extranonce)))
	out = append(out, extranonce...)
	putU32(&out, 0) // group channel id
	return out
}

func encodeSV2OpenChannelErrorPayload(requestID uint32, code string) []byte {
	out := make([]byte, 0, 64)
	putU32(&out, requestID)
	putStr0_255(&out, code)
	return out
}

func encodeSV2SetTargetPayload(channelID uint32, targetLE [32]byte) []byte {
	out := make([]byte, 0, 36)
	putU32(&out, channelID)
	out = append(out, targetLE[:]...)
	return out
}

func encodeSV2NewMiningJobPayload(channelID, jobID uint32, version uint32, prePowHash [32]byte) []byte {
	out := make([]byte, 0, 45)
	putU32(&out, channelID)
	putU32(&out, jobID)
	out = append(out, 0) // future_job=false
	putU32(&out, version)
	out = append(out, prePowHash[:]...)
	return out
}

func encodeSV2SetNewPrevHashPayload(channelID, jobID uint32, prevHash [32]byte, minNTime uint32, nBits uint32) []byte {
	out := make([]byte, 0, 48)
	putU32(&out, channelID)
	putU32(&out, jobID)
	out = append(out, prevHash[:]...)
	putU32(&out, minNTime)
	putU32(&out, nBits)
	return out
}

func encodeSV2SubmitSuccessPayload(channelID, sequence uint32) []byte {
	out := make([]byte, 0, 8)
	putU32(&out, channelID)
	putU32(&out, sequence)
	return out
}

func encodeSV2SubmitErrorPayload(channelID, sequence uint32, code string) []byte {
	out := make([]byte, 0, 64)
	putU32(&out, channelID)
	putU32(&out, sequence)
	putStr0_255(&out, code)
	return out
}

func diffToSV2TargetLE(diff float64) [32]byte {
	target := DiffToTarget(math.Max(diff, 0.00001))
	be := target.Bytes()
	var le [32]byte
	for i := 0; i < len(be) && i < len(le); i++ {
		le[i] = be[len(be)-1-i]
	}
	return le
}

func bytesToFixed32(input []byte) ([32]byte, error) {
	var out [32]byte
	if len(input) != 32 {
		return out, fmt.Errorf("expected 32-byte hash, got %d", len(input))
	}
	copy(out[:], input)
	return out, nil
}
