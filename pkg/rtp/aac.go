package rtp

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"
)

const (
	// AAC constants
	AACClockRate = 48000
	AUTime       = 1024 // Samples per AAC frame
)

// AACProcessor handles AAC RTP depacketization
type AACProcessor struct {
	OnFrame func(frame []byte) // Called when a complete AAC frame is ready
}

// NewAACProcessor creates a new AAC RTP processor
func NewAACProcessor() *AACProcessor {
	return &AACProcessor{}
}

// ProcessPacket processes an RTP packet containing AAC data
// AAC is typically sent using RFC 3640 (MPEG-4 Audio)
func (p *AACProcessor) ProcessPacket(packet *rtp.Packet) error {
	if len(packet.Payload) < 2 {
		return fmt.Errorf("AAC packet too short")
	}

	payload := packet.Payload

	// RFC 3640: AU-headers-length (16 bits) followed by AU headers
	auHeadersLength := binary.BigEndian.Uint16(payload[:2])
	auHeadersLengthBytes := (auHeadersLength + 7) / 8

	if len(payload) < int(2+auHeadersLengthBytes) {
		return fmt.Errorf("AAC packet malformed")
	}

	// For mode=AAC-hbr with sizelength=13, indexlength=3, indexdeltalength=3
	// Each AU header is 16 bits: 13 bits size + 3 bits index
	auHeaders := payload[2 : 2+auHeadersLengthBytes]
	auData := payload[2+auHeadersLengthBytes:]

	// Process each AU (Access Unit)
	offset := 0
	for len(auHeaders) >= 2 {
		// Extract AU size (13 bits, shifted right by 3)
		auSize := int(binary.BigEndian.Uint16(auHeaders[:2]) >> 3)

		if offset+auSize > len(auData) {
			break
		}

		frame := auData[offset : offset+auSize]
		offset += auSize

		// Emit frame
		if p.OnFrame != nil && len(frame) > 0 {
			p.OnFrame(frame)
		}

		// Move to next AU header (2 bytes per header)
		if len(auHeaders) >= 2 {
			auHeaders = auHeaders[2:]
		} else {
			break
		}
	}

	return nil
}
