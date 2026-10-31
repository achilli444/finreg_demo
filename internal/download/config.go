package download

import (
	"time"

	"github.com/moov-io/watchman/pkg/search"
)

type Config struct {
	RefreshInterval      time.Duration
	InitialDataDirectory string

	// ErrorOnEmptyList determines whether Watchman should raise an error when a list
	// becomes empty during refresh or download operations. An empty list could indicate:
	//   - A parsing error in the list processing logic
	//   - A corrupted or invalid downloaded file
	//   - Network or storage issues during file transfer
	//   - Other issues requiring manual investigation
	//
	// Setting this to true enables early detection of potential data integrity problems.
	// Default: false
	//
	// Use the ERROR_ON_EMPTY_LIST environment variable to set this at runtime.
	ErrorOnEmptyList bool

	// IncludedLists names the source lists to download and screen against
	// (us_ofac, us_non_sdn, eu_csl, etc). The shipped config.default.yml sets
	// us_ofac and us_non_sdn so that OFAC SDN and Non-SDN (SSI, etc) entries are
	// always part of the corpus. Entries from the INCLUDED_LISTS environment
	// variable are merged in.
	//
	// RefreshAll fails when no lists are configured at all (see
	// AllowEmptyIncludedLists), so a misconfigured deployment cannot report
	// clean "no match" results against an empty corpus.
	IncludedLists []search.SourceList

	// AllowEmptyIncludedLists opts out of the empty-corpus guard above. Set it
	// only for deployments that intentionally run with no downloaded lists, for
	// example an ingest-only instance that screens against uploaded files.
	// Default: false
	//
	// Use the ALLOW_EMPTY_INCLUDED_LISTS environment variable to set this at runtime.
	AllowEmptyIncludedLists bool

	// IgnoredDownloadErrors lists source-list names for which download or parse
	// errors are suppressed during RefreshAll. A failed list that is named here
	// will produce a warning log but will not cause RefreshAll to return an error.
	//
	// Valid values are the standard known downloadable lists plus any source-list
	// names configured in OpenSanctions.Lists or Senzing.
	//
	// Use the IGNORED_DOWNLOAD_ERRORS environment variable to set this at runtime
	// using a comma-separated list of source-list names.
	IgnoredDownloadErrors []search.SourceList

	OpenSanctions OpenSanctionsConfig
	Senzing       []SenzingList
}

type OpenSanctionsConfig struct {
	ApiKey string
	Lists  []SenzingList
}

type SenzingList struct {
	SourceList search.SourceList
	Location   string
}
