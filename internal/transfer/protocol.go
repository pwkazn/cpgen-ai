package transfer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const (
	FrameSchemaVersion      domain.SchemaVersion = "cpgen.transfer-frame/v1"
	ExportPlanSchemaVersion domain.SchemaVersion = "cpgen.transfer-export-plan/v1"
	maxFrameHeaderBytes                          = 64 << 10
)

type FrameMode string

const FrameModeRegular FrameMode = "REGULAR"

const FrameModeExecutable FrameMode = "EXECUTABLE"

func (m FrameMode) Valid() bool { return m == FrameModeRegular || m == FrameModeExecutable }

func (m *FrameMode) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode frame mode: %w", err)
	}
	if !FrameMode(raw).Valid() {
		return fmt.Errorf("unknown frame mode %q", raw)
	}
	*m = FrameMode(raw)
	return nil
}

type FrameHeader struct {
	SchemaVersion domain.SchemaVersion `json:"schema_version"`
	Path          domain.SafeRelPath   `json:"path"`
	Size          int64                `json:"size"`
	Mode          FrameMode            `json:"mode"`
}

func (h FrameHeader) Validate() error {
	if h.SchemaVersion != FrameSchemaVersion {
		return fmt.Errorf("frame schema version must be %q", FrameSchemaVersion)
	}
	if err := h.Path.Validate(); err != nil {
		return err
	}
	if h.Size < 0 {
		return fmt.Errorf("frame size must be non-negative")
	}
	if !h.Mode.Valid() {
		return fmt.Errorf("invalid frame mode %q", h.Mode)
	}
	return nil
}

type ImportLimits struct {
	MaxFiles      int
	MaxTotalBytes int64
}

func (l ImportLimits) Validate() error {
	if l.MaxFiles <= 0 {
		return fmt.Errorf("maximum file count must be positive")
	}
	if l.MaxTotalBytes < 0 {
		return fmt.Errorf("maximum total bytes must be non-negative")
	}
	return nil
}

type ImportedFile struct {
	Path   domain.SafeRelPath
	Digest domain.Digest
	Size   int64
	Mode   FrameMode
}

type ExportedFile struct {
	Path   domain.SafeRelPath
	Digest domain.Digest
	Size   int64
	Mode   FrameMode
}

type ExportPlan struct {
	SchemaVersion domain.SchemaVersion     `json:"schema_version"`
	Files         []port.OutputDeclaration `json:"files"`
	MaxFiles      int                      `json:"max_files"`
	MaxTotalBytes int64                    `json:"max_total_bytes"`
}

func (p ExportPlan) Validate() error {
	if p.SchemaVersion != ExportPlanSchemaVersion {
		return fmt.Errorf("export plan schema version must be %q", ExportPlanSchemaVersion)
	}
	if p.MaxFiles <= 0 || p.MaxTotalBytes < 0 {
		return fmt.Errorf("export plan limits are invalid")
	}
	if len(p.Files) > p.MaxFiles {
		return fmt.Errorf("export plan declares %d files above limit %d", len(p.Files), p.MaxFiles)
	}
	seen := make(map[domain.SafeRelPath]struct{}, len(p.Files))
	for index, file := range p.Files {
		if err := file.Path.Validate(); err != nil {
			return fmt.Errorf("export file %d: %w", index, err)
		}
		if file.MaxBytes <= 0 {
			return fmt.Errorf("export file %d must have a positive byte limit", index)
		}
		if _, exists := seen[file.Path]; exists {
			return fmt.Errorf("duplicate export path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
	}
	return nil
}

func ParseExportPlan(data []byte) (ExportPlan, error) {
	var plan ExportPlan
	if err := decodeStrictJSON(data, &plan); err != nil {
		return ExportPlan{}, err
	}
	if err := plan.Validate(); err != nil {
		return ExportPlan{}, err
	}
	return plan, nil
}

func readFrameHeader(reader io.Reader) (FrameHeader, bool, error) {
	var lengthBytes [4]byte
	if _, err := io.ReadFull(reader, lengthBytes[:]); err != nil {
		return FrameHeader{}, false, fmt.Errorf("read frame header length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthBytes[:])
	if length == 0 {
		return FrameHeader{}, true, nil
	}
	if length > maxFrameHeaderBytes {
		return FrameHeader{}, false, fmt.Errorf("frame header length %d exceeds limit %d", length, maxFrameHeaderBytes)
	}
	headerBytes := make([]byte, int(length))
	if _, err := io.ReadFull(reader, headerBytes); err != nil {
		return FrameHeader{}, false, fmt.Errorf("read frame header: %w", err)
	}
	var header FrameHeader
	if err := decodeStrictJSON(headerBytes, &header); err != nil {
		return FrameHeader{}, false, fmt.Errorf("decode frame header: %w", err)
	}
	if err := header.Validate(); err != nil {
		return FrameHeader{}, false, err
	}
	var dataLengthBytes [8]byte
	if _, err := io.ReadFull(reader, dataLengthBytes[:]); err != nil {
		return FrameHeader{}, false, fmt.Errorf("read frame data length: %w", err)
	}
	dataLength := binary.BigEndian.Uint64(dataLengthBytes[:])
	if dataLength > math.MaxInt64 || int64(dataLength) != header.Size {
		return FrameHeader{}, false, fmt.Errorf("frame data length %d does not match declared size %d", dataLength, header.Size)
	}
	return header, false, nil
}

func writeFrameHeader(writer io.Writer, header FrameHeader) error {
	if err := header.Validate(); err != nil {
		return err
	}
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("encode frame header: %w", err)
	}
	if len(headerBytes) > maxFrameHeaderBytes {
		return fmt.Errorf("encoded frame header is too large")
	}
	var lengthBytes [4]byte
	binary.BigEndian.PutUint32(lengthBytes[:], uint32(len(headerBytes)))
	if _, err := writer.Write(lengthBytes[:]); err != nil {
		return fmt.Errorf("write frame header length: %w", err)
	}
	if _, err := writer.Write(headerBytes); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	var dataLengthBytes [8]byte
	binary.BigEndian.PutUint64(dataLengthBytes[:], uint64(header.Size))
	if _, err := writer.Write(dataLengthBytes[:]); err != nil {
		return fmt.Errorf("write frame data length: %w", err)
	}
	return nil
}

func writeTerminator(writer io.Writer) error {
	var terminator [4]byte
	if _, err := writer.Write(terminator[:]); err != nil {
		return fmt.Errorf("write frame terminator: %w", err)
	}
	return nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains a trailing value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
