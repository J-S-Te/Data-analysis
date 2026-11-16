package metricfacts

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"
)

const MaxFrameBytes = 1 << 20
const MaxChunkRows = 256
const MaxRowBytes = 64 << 10

// streamEnd 保存源事务成功提交后发出的完整性凭据。
type streamEnd struct {
	Type                string `json:"type"`
	RowCount            int64  `json:"row_count"`
	SHA256              string `json:"sha256"`
	SourceUpdatedAt     string `json:"source_updated_at,omitempty"`
	CollectionStartedAt string `json:"collection_started_at,omitempty"`
}

// strictJSON 拒绝未知字段、额外 JSON 值和非规范行对象。
// 参数：raw 为有界 JSON，target 为唯一预期结构。
// 返回值：严格解码成功时 nil，否则返回来源格式错误。
func strictJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}

// streamLine 逐段读取单个 LF 帧，避免上游无限长行造成无界分配。
// 参数：reader 为固定缓冲区输入。
// 返回值：不含 LF 的完整行；没有尾行时 EOF，超长或截断时错误。
func streamLine(reader *bufio.Reader) ([]byte, error) {
	raw := make([]byte, 0, 4096)
	for {
		part, err := reader.ReadSlice('\n')
		if len(raw)+len(part) > MaxFrameBytes+1 {
			return nil, ErrInvalid
		}
		raw = append(raw, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(raw) == 0 {
			return nil, io.EOF
		}
		if err != nil || len(raw) <= 1 || raw[len(raw)-1] != '\n' {
			return nil, ErrInvalid
		}
		return raw[:len(raw)-1], nil
	}
}

// readStream 校验有界帧并按块移交暂存，不在内存保留全量事实。
// 参数：reader 为来源流，tenant/source/now 为可信边界，begin/accept 为暂存回调。
// 返回值：完整元数据、事实数及摘要；只有 end 后 EOF 才视为成功。
func readStream(reader io.Reader, tenant, source string, now time.Time,
	begin func(Bundle) error, accept func([]Row) error) (Bundle, int64, string, error) {
	input := bufio.NewReaderSize(reader, 64<<10)
	raw, err := streamLine(input)
	if err != nil {
		return Bundle{}, 0, "", ErrInvalid
	}
	var first struct {
		Type     string `json:"type"`
		Metadata Bundle `json:"metadata"`
	}
	if strictJSON(raw, &first) != nil || first.Type != "begin" || len(first.Metadata.Rows) != 0 ||
		Validate(first.Metadata, tenant, source, now) != nil {
		return Bundle{}, 0, "", ErrInvalid
	}
	if err := begin(first.Metadata); err != nil {
		return Bundle{}, 0, "", err
	}
	hash := sha256.New()
	var count int64
	sequence := int64(1)
	for {
		raw, err = streamLine(input)
		if err != nil {
			return Bundle{}, 0, "", ErrInvalid
		}
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &kind) != nil {
			return Bundle{}, 0, "", ErrInvalid
		}
		if kind.Type == "end" {
			var end streamEnd
			if strictJSON(raw, &end) != nil || end.RowCount != count ||
				end.SHA256 != hex.EncodeToString(hash.Sum(nil)) {
				return Bundle{}, 0, "", ErrInvalid
			}
			first.Metadata.SourceUpdatedAt = end.SourceUpdatedAt
			first.Metadata.CollectionStartedAt = end.CollectionStartedAt
			if Validate(first.Metadata, tenant, source, now) != nil {
				return Bundle{}, 0, "", ErrInvalid
			}
			if _, err := input.ReadByte(); err != io.EOF {
				return Bundle{}, 0, "", ErrInvalid
			}
			return first.Metadata, count, end.SHA256, nil
		}
		var chunk struct {
			Type     string            `json:"type"`
			Sequence int64             `json:"sequence"`
			Rows     []json.RawMessage `json:"rows"`
		}
		if kind.Type != "chunk" || strictJSON(raw, &chunk) != nil || chunk.Sequence != sequence ||
			len(chunk.Rows) == 0 || len(chunk.Rows) > MaxChunkRows {
			return Bundle{}, 0, "", ErrInvalid
		}
		rows := make([]Row, 0, len(chunk.Rows))
		for _, item := range chunk.Rows {
			var compact bytes.Buffer
			var row Row
			if len(item) > MaxRowBytes || json.Compact(&compact, item) != nil || !bytes.Equal(item, compact.Bytes()) ||
				strictJSON(item, &row) != nil {
				return Bundle{}, 0, "", ErrInvalid
			}
			candidate := first.Metadata
			candidate.Rows = []Row{row}
			if Validate(candidate, tenant, source, now) != nil {
				return Bundle{}, 0, "", ErrInvalid
			}
			rows = append(rows, row)
			_, _ = hash.Write(item)
			_, _ = hash.Write([]byte{'\n'})
		}
		if err := accept(rows); err != nil {
			return Bundle{}, 0, "", err
		}
		if count > MaxInteger-int64(len(rows)) {
			return Bundle{}, 0, "", ErrInvalid
		}
		count += int64(len(rows))
		sequence++
	}
}
