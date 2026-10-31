package ingest

import (
	"os"
	"strconv"
	"strings"
)

const (
	defaultMaxBodyBytes int64 = 32 << 20 // 32 MiB

	// defaultDecompressionRatio bounds how far a gzip body may expand relative to maxBodyBytes.
	defaultDecompressionRatio int64 = 8

	defaultMaxEntities = 1_000_000
)

type Config struct {
	Files map[string]File

	// PaginationLimit controls the batch size when listing entities from the database.
	// Defaults to 1000 if not set.
	PaginationLimit int

	// MaxBodyBytes is the maximum POST /v2/ingest/{fileType} request body size.
	// Zero or negative uses defaultMaxBodyBytes (32 MiB). Override with INGEST_MAX_BODY_BYTES.
	MaxBodyBytes int64

	// MaxDecompressedBytes is the maximum size a gzip request body may expand to.
	// Zero or negative uses 8x MaxBodyBytes. Override with INGEST_MAX_DECOMPRESSED_BYTES.
	MaxDecompressedBytes int64

	// MaxEntities is the maximum number of rows a single upload may contain.
	// Zero or negative uses defaultMaxEntities. Override with INGEST_MAX_ENTITIES.
	MaxEntities int

	// AllowedOrigins lists browser origins (e.g. https://ops.example.com) that may call the
	// ingest and export endpoints cross-origin. Empty means no cross-origin browser access.
	// Override with a comma separated INGEST_ALLOWED_ORIGINS.
	AllowedOrigins []string
}

func (c Config) maxBodyBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("INGEST_MAX_BODY_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	if c.MaxBodyBytes > 0 {
		return c.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

func (c Config) maxDecompressedBytes() int64 {
	if n := positiveEnvInt64("INGEST_MAX_DECOMPRESSED_BYTES"); n > 0 {
		return n
	}
	if c.MaxDecompressedBytes > 0 {
		return c.MaxDecompressedBytes
	}
	return c.maxBodyBytes() * defaultDecompressionRatio
}

func (c Config) maxEntities() int {
	if n := positiveEnvInt64("INGEST_MAX_ENTITIES"); n > 0 {
		return int(n)
	}
	if c.MaxEntities > 0 {
		return c.MaxEntities
	}
	return defaultMaxEntities
}

func (c Config) allowedOrigins() []string {
	if v := strings.TrimSpace(os.Getenv("INGEST_ALLOWED_ORIGINS")); v != "" {
		var out []string
		for _, origin := range strings.Split(v, ",") {
			if origin = strings.TrimSpace(origin); origin != "" {
				out = append(out, origin)
			}
		}
		return out
	}
	return c.AllowedOrigins
}

// originAllowed reports whether a browser Origin is on the configured allowlist.
func (c Config) originAllowed(origin string) bool {
	origin = strings.TrimSuffix(strings.TrimSpace(origin), "/")
	if origin == "" || origin == "null" {
		return false
	}
	for _, allowed := range c.allowedOrigins() {
		if strings.EqualFold(strings.TrimSuffix(allowed, "/"), origin) {
			return true
		}
	}
	return false
}

func positiveEnvInt64(name string) int64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

type File struct {
	Format  Format
	Mapping Mapping
}

type Format string

var (
	FormatCSV Format = "csv"

	// Senzing entity format support
	// Reference: https://www.senzing.com/docs/entity_specification/index.html
	FormatSenzing      Format = "senzing"       // auto-detect JSON Lines or JSON Array
	FormatSenzingJSON  Format = "senzing-json"  // JSON Array format
	FormatSenzingJSONL Format = "senzing-jsonl" // JSON Lines format
)

type Mapping struct {
	Name     ColumnDef
	SourceID ColumnDef
	Type     Type

	Person   *Person
	Business *Business

	Contact   Contact
	Addresses Addresses
}

type ColumnDef struct {
	Column string
	Merge  []string
}

type ColumnArrayDef struct {
	Columns string
	Merge   []string
}

type Type struct {
	Default string
}

type Person struct {
	Name          ColumnDef
	AltNames      ColumnArrayDef
	BirthDate     ColumnDef
	GovernmentIDs GovernmentIDs
}

type Business struct {
	Name          ColumnDef
	AltNames      ColumnArrayDef
	Created       ColumnDef
	GovernmentIDs GovernmentIDs
}

type GovernmentIDs struct {
	Type       ColumnDef
	Identifier ColumnDef
}

type Contact struct {
	PhoneNumbers ColumnArrayDef
}

type Addresses struct {
	Line1      ColumnArrayDef
	Line2      ColumnArrayDef
	City       ColumnArrayDef
	State      ColumnArrayDef
	PostalCode ColumnArrayDef
	Country    ColumnArrayDef
}
