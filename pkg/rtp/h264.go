package rtp

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"
)

const (
	// NAL Unit types
	NALUTypeUnspecified = 0
	NALUTypePFrame      = 1
	NALUTypeIFrame      = 5
	NALUTypeSEI         = 6
	NALUTypeSPS         = 7
	NALUTypePPS         = 8
	NALUTypeAUD         = 9
	NALUTypeSTAPA       = 24 // Single-Time Aggregation Packet
	NALUTypeFUA         = 28 // Fragmentation Unit A
)

// H264Processor handles H.264 RTP depacketization
type H264Processor struct {
	buffer   []byte // Buffer for accumulating fragmented NALUs
	sps      []byte
	pps      []byte
	OnFrame  func(nalus []byte, keyframe bool) // Called when a complete frame is ready
}

// NewH264Processor creates a new H.264 RTP processor
func NewH264Processor() *H264Processor {
	return &H264Processor{
		buffer: make([]byte, 0, 1024*1024), // 1MB initial buffer
	}
}

// ProcessPacket processes an RTP packet containing H.264 data
func (p *H264Processor) ProcessPacket(packet *rtp.Packet) error {
	if len(packet.Payload) == 0 {
		return nil
	}

	payload := packet.Payload
	naluType := payload[0] & 0x1F

	switch naluType {
	case NALUTypeFUA:
		// Fragmentation Unit
		return p.processFUA(packet)

	case NALUTypeSTAPA:
		// Single-Time Aggregation Packet
		return p.processSTAPA(packet)

	default:
		// Single NAL Unit
		return p.processSingleNALU(packet)
	}
}

// processFUA handles fragmented NAL units (FU-A)
func (p *H264Processor) processFUA(packet *rtp.Packet) error {
	if len(packet.Payload) < 2 {
		return fmt.Errorf("FU-A packet too short")
	}

	fuIndicator := packet.Payload[0]
	fuHeader := packet.Payload[1]
	payload := packet.Payload[2:]

	start := (fuHeader & 0x80) != 0
	end := (fuHeader & 0x40) != 0
	naluType := fuHeader & 0x1F

	if start {
		// Start of fragmented NALU
		p.buffer = p.buffer[:0]

		// Reconstruct NAL header
		nalHeader := (fuIndicator & 0xE0) | naluType
		p.buffer = append(p.buffer, nalHeader)
	}

	// Append fragment
	p.buffer = append(p.buffer, payload...)

	if end {
		// End of fragmented NALU - emit complete NALU
		return p.emitNALU(p.buffer, naluType, packet.Marker)
	}

	return nil
}

// processSTAPA handles aggregated packets
func (p *H264Processor) processSTAPA(packet *rtp.Packet) error {
	payload := packet.Payload[1:] // Skip STAP-A header

	nalus := make([]byte, 0, len(payload)*2)

	for len(payload) > 2 {
		// Read NALU size (2 bytes, big endian)
		naluSize := binary.BigEndian.Uint16(payload[:2])
		payload = payload[2:]

		if len(payload) < int(naluSize) {
			return fmt.Errorf("STAP-A NALU size exceeds payload")
		}

		nalu := payload[:naluSize]
		payload = payload[naluSize:]

		// Add to aggregated NALUs with length prefix
		nalus = appendNALU(nalus, nalu)

		// Extract SPS/PPS for later use
		naluType := nalu[0] & 0x1F
		if naluType == NALUTypeSPS {
			p.sps = make([]byte, len(nalu))
			copy(p.sps, nalu)
		} else if naluType == NALUTypePPS {
			p.pps = make([]byte, len(nalu))
			copy(p.pps, nalu)
		}
	}

	if len(nalus) > 0 && p.OnFrame != nil {
		p.OnFrame(nalus, false)
	}

	return nil
}

// processSingleNALU handles single NAL units
func (p *H264Processor) processSingleNALU(packet *rtp.Packet) error {
	nalu := packet.Payload
	naluType := nalu[0] & 0x1F

	return p.emitNALU(nalu, naluType, packet.Marker)
}

// emitNALU emits a complete NALU
func (p *H264Processor) emitNALU(nalu []byte, naluType uint8, marker bool) error {
	// Store SPS/PPS for later
	if naluType == NALUTypeSPS {
		p.sps = make([]byte, len(nalu))
		copy(p.sps, nalu)
	} else if naluType == NALUTypePPS {
		p.pps = make([]byte, len(nalu))
		copy(p.pps, nalu)
	}

	// For keyframes, prepend SPS/PPS
	var frame []byte
	isKeyframe := naluType == NALUTypeIFrame

	if isKeyframe && len(p.sps) > 0 && len(p.pps) > 0 {
		frame = make([]byte, 0, len(p.sps)+len(p.pps)+len(nalu)+12)
		frame = appendNALU(frame, p.sps)
		frame = appendNALU(frame, p.pps)
		frame = appendNALU(frame, nalu)
	} else {
		frame = make([]byte, 0, len(nalu)+4)
		frame = appendNALU(frame, nalu)
	}

	if p.OnFrame != nil && marker {
		p.OnFrame(frame, isKeyframe)
	}

	return nil
}

// appendNALU appends a NALU with length prefix (AVC format)
func appendNALU(dst, nalu []byte) []byte {
	// AVC format: 4-byte length prefix + NALU data
	length := uint32(len(nalu))
	dst = append(dst,
		byte(length>>24),
		byte(length>>16),
		byte(length>>8),
		byte(length),
	)
	return append(dst, nalu...)
}

// GetSPS returns the stored SPS
func (p *H264Processor) GetSPS() []byte {
	return p.sps
}

// GetPPS returns the stored PPS
func (p *H264Processor) GetPPS() []byte {
	return p.pps
}
