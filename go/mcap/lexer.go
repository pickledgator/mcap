package mcap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// ErrNestedChunk indicates the lexer has detected a nested chunk.
var ErrNestedChunk = errors.New("detected nested chunk")
var ErrChunkTooLarge = errors.New("chunk exceeds configured maximum size")
var ErrRecordTooLarge = errors.New("record exceeds configured maximum size")

type errInvalidChunkCrc struct {
	expected uint32
	actual   uint32
}

func (e *errInvalidChunkCrc) Error() string {
	return fmt.Sprintf("invalid chunk CRC: %x != %x", e.actual, e.expected)
}

// ErrBadMagic indicates the lexer has detected invalid magic bytes.
var ErrBadMagic = errors.New("not an MCAP file")

const (
	// TokenHeader represents a header token.
	TokenHeader TokenType = iota
	// TokenFooter represents a footer token.
	TokenFooter
	// TokenSchema represents a schema token.
	TokenSchema
	// TokenChannel represents a channel token.
	TokenChannel
	// TokenMessage represents a message token.
	TokenMessage
	// TokenChunk represents a chunk token.
	TokenChunk
	// TokenMessageIndex represents a message index token.
	TokenMessageIndex
	// TokenChunkIndex represents a chunk index token.
	TokenChunkIndex
	// TokenAttachment represents an attachment token.
	TokenAttachment
	// TokenAttachmentIndex represents an attachment index token.
	TokenAttachmentIndex
	// TokenStatistics represents a statistics token.
	TokenStatistics
	// TokenMetadata represents a metadata token.
	TokenMetadata
	// TokenSummaryOffset represents a summary offset token.
	TokenMetadataIndex
	// TokenDataEnd represents a data end token.
	TokenSummaryOffset
	// 	TokenError represents an error token.
	TokenDataEnd
	// TokenError represents an error token.
	TokenError
	// TokenInvalidChunk represents a chunk token that failed CRC validation.
	TokenInvalidChunk
)

// TokenType encodes a type of token from the lexer.
type TokenType int

// String converts a token type to its string representation.
func (t TokenType) String() string {
	switch t {
	case TokenHeader:
		return "header"
	case TokenFooter:
		return "footer"
	case TokenSchema:
		return "schema"
	case TokenChannel:
		return "channel"
	case TokenMessage:
		return "message"
	case TokenChunk:
		return "chunk"
	case TokenMessageIndex:
		return "message index"
	case TokenChunkIndex:
		return "chunk index"
	case TokenAttachment:
		return "attachment"
	case TokenAttachmentIndex:
		return "attachment index"
	case TokenStatistics:
		return "statistics"
	case TokenMetadata:
		return "metadata"
	case TokenSummaryOffset:
		return "summary offset"
	case TokenDataEnd:
		return "data end"
	case TokenError:
		return "error"
	case TokenInvalidChunk:
		return "invalid chunk"
	default:
		return "unknown"
	}
}

// Lexer is a low-level reader for mcap files that emits tokenized byte strings
// without parsing or interpreting them, except in the case of chunks, which may
// be optionally de-chunked.
type Lexer struct {
	basereader io.Reader
	reader     io.Reader
	emitChunks bool

	decoders                 decoders
	inChunk                  bool
	buf                      []byte
	uncompressedChunk        []byte
	validateCRC              bool
	emitInvalidChunks        bool
	maxRecordSize            int
	maxDecompressedChunkSize int
}

// Next returns the next token from the lexer as a byte array. The result will
// be sliced out of the provided buffer `p`, if p has adequate space. If p does
// not have adequate space, a new buffer with sufficient size is allocated for
// the result.
func (l *Lexer) Next(p []byte) (TokenType, []byte, error) {
	for {
		_, err := io.ReadFull(l.reader, l.buf[:9])
		if err != nil {
			unexpectedEOF := errors.Is(err, io.ErrUnexpectedEOF)
			eof := errors.Is(err, io.EOF)
			if l.inChunk && (eof || unexpectedEOF) {
				l.inChunk = false
				l.reader = l.basereader
				continue
			}
			if unexpectedEOF || eof {
				return TokenError, nil, io.EOF
			}
			return TokenError, nil, err
		}
		opcode := OpCode(l.buf[0])
		recordLen := binary.LittleEndian.Uint64(l.buf[1:9])
		if l.maxRecordSize > 0 && recordLen > uint64(l.maxRecordSize) {
			return TokenError, nil, ErrRecordTooLarge
		}
		if opcode == OpChunk && !l.emitChunks {
			err := loadChunk(l)
			if err != nil {
				if l.emitInvalidChunks {
					var invalidCrc *errInvalidChunkCrc
					if errors.As(err, &invalidCrc) {
						return TokenInvalidChunk, nil, err
					}
				}
				return TokenError, nil, err
			}
			continue
		}

		if recordLen > uint64(len(p)) {
			p, err = makeSafe(recordLen)
			if err != nil {
				return TokenError, nil, fmt.Errorf("failed to allocate %d bytes for %s token: %w", recordLen, opcode, err)
			}
		}

		record := p[:recordLen]
		_, err = io.ReadFull(l.reader, record)
		if err != nil {
			return TokenError, nil, err
		}

		switch opcode {
		case OpMessage:
			return TokenMessage, record, nil
		case OpHeader:
			return TokenHeader, record, nil
		case OpSchema:
			return TokenSchema, record, nil
		case OpDataEnd:
			return TokenDataEnd, record, nil
		case OpChannel:
			return TokenChannel, record, nil
		case OpFooter:
			return TokenFooter, record, nil
		case OpAttachment:
			return TokenAttachment, record, nil
		case OpAttachmentIndex:
			return TokenAttachmentIndex, record, nil
		case OpChunkIndex:
			return TokenChunkIndex, record, nil
		case OpStatistics:
			return TokenStatistics, record, nil
		case OpMessageIndex:
			return TokenMessageIndex, record, nil
		case OpChunk:
			return TokenChunk, record, nil
		case OpMetadata:
			return TokenMetadata, record, nil
		case OpMetadataIndex:
			return TokenMetadataIndex, record, nil
		case OpSummaryOffset:
			return TokenSummaryOffset, record, nil
		case OpReserved:
			return TokenError, nil, fmt.Errorf("invalid zero opcode")
		default:
			continue // skip unrecognized opcodes
		}
	}
}

type decoders struct {
	zstd *zstd.Decoder
	lz4  *lz4.Reader
	none *bytes.Reader
}

func validateMagic(r io.Reader) error {
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return ErrBadMagic
	}
	if !bytes.Equal(magic, Magic) {
		return ErrBadMagic
	}
	return nil
}

func (l *Lexer) setNoneDecoder(buf []byte) {
	if l.decoders.none == nil {
		l.decoders.none = bytes.NewReader(buf)
	} else {
		l.decoders.none.Reset(buf)
	}
	l.reader = l.decoders.none
}

func (l *Lexer) setZSTDDecoder(r io.Reader) error {
	if l.decoders.zstd == nil {
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return err
		}
		l.decoders.zstd = decoder
	} else {
		err := l.decoders.zstd.Reset(r)
		if err != nil {
			return err
		}
	}
	l.reader = l.decoders.zstd
	return nil
}

func (l *Lexer) setLZ4Decoder(r io.Reader) {
	if l.decoders.lz4 == nil {
		l.decoders.lz4 = lz4.NewReader(r)
	} else {
		l.decoders.lz4.Reset(r)
	}
	l.reader = l.decoders.lz4
}

func loadChunk(l *Lexer) error {
	if l.inChunk {
		return ErrNestedChunk
	}
	_, err := io.ReadFull(l.reader, l.buf[:8+8+8+4+4])
	if err != nil {
		return err
	}
	_, offset, err := getUint64(l.buf, 0) // start
	if err != nil {
		return fmt.Errorf("failed to read start: %w", err)
	}
	_, offset, err = getUint64(l.buf, offset) // end
	if err != nil {
		return fmt.Errorf("failed to read end: %w", err)
	}
	uncompressedSize, offset, err := getUint64(l.buf, offset)
	if err != nil {
		return fmt.Errorf("failed to read uncompressed size: %w", err)
	}
	uncompressedCRC, offset, err := getUint32(l.buf, offset)
	if err != nil {
		return fmt.Errorf("failed to read uncompressed CRC: %w", err)
	}
	compressionLen, _, err := getUint32(l.buf, offset)
	if err != nil {
		return fmt.Errorf("failed to read compression length: %w", err)
	}

	// read compression and records length into buffer
	_, err = io.ReadFull(l.reader, l.buf[:compressionLen+8])
	if err != nil {
		return fmt.Errorf("failed to read compression from chunk: %w", err)
	}
	compression := CompressionFormat(l.buf[:compressionLen])
	recordsLength, _, err := getUint64(l.buf, int(compressionLen))
	if err != nil {
		return fmt.Errorf("failed to read records length: %w", err)
	}

	// remaining bytes in the record are the chunk data
	lr := io.LimitReader(l.reader, int64(recordsLength))
	switch compression {
	case CompressionNone:
		l.reader = lr
	case CompressionZSTD:
		err = l.setZSTDDecoder(lr)
		if err != nil {
			return err
		}
	case CompressionLZ4:
		l.setLZ4Decoder(lr)
	default:
		return fmt.Errorf("unsupported compression: %s", string(compression))
	}
	l.inChunk = true

	// if we are validating the CRC, we need to fully decompress the chunk right
	// here, then rewrap the decompressed data in a compatible reader after
	// validation. If we are not validating CRCs, we can use incremental
	// decompression for the chunk's data, which may be beneficial to streaming
	// readers.
	if l.validateCRC {
		if l.maxDecompressedChunkSize > 0 && uncompressedSize > uint64(l.maxDecompressedChunkSize) {
			return ErrChunkTooLarge
		}
		if uint64(len(l.uncompressedChunk)) < uncompressedSize {
			l.uncompressedChunk, err = makeSafe(uncompressedSize * 2)
			if err != nil {
				return fmt.Errorf("failed to allocate chunk buffer: %w", err)
			}
		}

		_, err := io.ReadFull(l.reader, l.uncompressedChunk[:uncompressedSize])
		if err != nil {
			return fmt.Errorf("failed to decompress chunk: %w", err)
		}

		// LZ4 chunks may have some crc data at the end that is not required to
		// fill a buffer, meaning the ReadFull call above does not consume it.
		// Therefore we have to do an empty read. If we get any data out of
		// this, it's an error.
		if compression == CompressionLZ4 {
			extraBytes, err := io.ReadAll(l.reader)
			if err != nil {
				return fmt.Errorf("failed to read extra bytes: %w", err)
			}
			if len(extraBytes) > 0 {
				return fmt.Errorf("encountered unexpected bytes after chunk: %q", extraBytes)
			}
		}

		crc := crc32.ChecksumIEEE(l.uncompressedChunk[:uncompressedSize])
		if uncompressedCRC > 0 && crc != uncompressedCRC {
			return &errInvalidChunkCrc{expected: uncompressedCRC, actual: crc}
		}
		l.setNoneDecoder(l.uncompressedChunk[:uncompressedSize])
	}
	return nil
}

// LexerOptions holds options for the lexer.
type LexerOptions struct {
	// SkipMagic instructs the lexer not to perform validation of the leading magic bytes.
	SkipMagic bool
	// ValidateCRC instructs the lexer to validate CRC checksums for chunks.
	ValidateCRC bool
	// EmitChunks instructs the lexer to emit chunk records without de-chunking.
	// It is incompatible with ValidateCRC.
	EmitChunks bool
	// EmitChunks instructs the lexer to emit TokenInvalidChunk rather than TokenError when CRC
	// validation fails.
	EmitInvalidChunks bool
	// MaxDecompressedChunkSize defines the maximum size chunk the lexer will
	// decompress. Chunks larger than this will result in an error.
	MaxDecompressedChunkSize int
	// MaxRecordSize defines the maximum size record the lexer will read.
	// Records larger than this will result in an error.
	MaxRecordSize int
}

// NewLexer returns a new lexer for the given reader.
func NewLexer(r io.Reader, opts ...*LexerOptions) (*Lexer, error) {
	var maxRecordSize, maxDecompressedChunkSize int
	var validateCRC, emitChunks, emitInvalidChunks, skipMagic bool
	if len(opts) > 0 {
		validateCRC = opts[0].ValidateCRC
		emitChunks = opts[0].EmitChunks
		emitInvalidChunks = opts[0].EmitInvalidChunks
		skipMagic = opts[0].SkipMagic
		maxRecordSize = opts[0].MaxRecordSize
		maxDecompressedChunkSize = opts[0].MaxDecompressedChunkSize
	}
	if !skipMagic {
		err := validateMagic(r)
		if err != nil {
			return nil, err
		}
	}
	return &Lexer{
		basereader:               r,
		reader:                   r,
		buf:                      make([]byte, 32),
		validateCRC:              validateCRC,
		emitChunks:               emitChunks,
		emitInvalidChunks:        emitInvalidChunks,
		maxRecordSize:            maxRecordSize,
		maxDecompressedChunkSize: maxDecompressedChunkSize,
	}, nil
}
