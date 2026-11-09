package agentproto

import (
	"errors"
	"path"
	"strings"
	"time"
)

// Importing one website from an archive of its files (a .tar.gz, .tar or .zip from any
// host, aaPanel's site backups among them), with a database dump if it has one: a .sql or
// .sql.gz file next to it in ImportDir, or one inside the archive.

const (
	OpImportList        = "import.list"
	OpImportSiteInspect = "import.site_inspect"
	OpImportSiteApply   = "import.site_apply"
)

// ImportFileKinds: what a file in ImportDir can be used for.
const (
	ImportKindArchive = "archive" // .tar.gz, .tgz, .tar (a cPanel backup, or a website's files)
	ImportKindZip     = "zip"     // a website's files
	ImportKindDump    = "dump"    // .sql, .sql.gz
)

type ImportListResult struct {
	Files []ImportListedFile `json:"files"`
}

type ImportListedFile struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// ImportFileKind tells what a file name in ImportDir is, or "" for a name that cannot be used.
func ImportFileKind(name string) string {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") || len(name) > 200 {
		return ""
	}
	switch {
	case strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".tar"):
		return ImportKindArchive
	case strings.HasSuffix(name, ".zip"):
		return ImportKindZip
	case strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".sql.gz"):
		return ImportKindDump
	}
	return ""
}

type ImportSiteInspectParams struct {
	Archive string `json:"archive"`
}

// SitePlan is what a website archive holds.
type SitePlan struct {
	// Root is the folder inside the archive that is the website ("" for the top).
	Root  string `json:"root"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	// Kind is "wordpress", "php" or "static".
	Kind string `json:"kind"`
	// Domain is a guess from the file name (aaPanel: web_<domain>_<date>.tar.gz) or the
	// archive's top folder.
	Domain    string             `json:"domain,omitempty"`
	WordPress *WordPressSettings `json:"wordpress,omitempty"`
	// Dumps are database dumps inside the archive (outside the website, or in it).
	Dumps []ArchivedDump `json:"dumps"`
}

type ArchivedDump struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// WordPressSettings come from wp-config.php. PasswordHash is the MySQL native hash of
// DB_PASSWORD (the password itself stays on the server).
type WordPressSettings struct {
	DBName       string `json:"db_name"`
	DBUser       string `json:"db_user"`
	DBHost       string `json:"db_host"`
	PasswordHash string `json:"password_hash,omitempty"`
	TablePrefix  string `json:"table_prefix,omitempty"`
}

type ImportSiteApplyParams struct {
	Archive string `json:"archive"`
	Root    string `json:"root"`
	User    string `json:"user"` // the website's account: files go to its public_html
	// The database to fill, from Dump (a file in ImportDir) or DumpInArchive.
	Database      *TransferDatabase `json:"database,omitempty"`
	Dump          string            `json:"dump,omitempty"`
	DumpInArchive string            `json:"dump_in_archive,omitempty"`
	// WordPress: wp-config.php gets this database's name and user, and localhost.
	WordPress bool `json:"wordpress,omitempty"`
}

func (p ImportSiteApplyParams) Validate() error {
	if k := ImportFileKind(p.Archive); k != ImportKindArchive && k != ImportKindZip {
		return ErrInvalidImportFile
	}
	if p.Root != "" && (path.IsAbs(p.Root) || strings.Contains("/"+p.Root+"/", "/../")) || !linuxUserRE.MatchString(p.User) {
		return errors.New("invalid website")
	}
	if p.Dump != "" && ImportFileKind(p.Dump) != ImportKindDump || p.Dump != "" && p.DumpInArchive != "" {
		return errors.New("invalid dump")
	}
	if p.Database != nil {
		return validTransferDBs([]TransferDatabase{*p.Database}, true)
	}
	if p.Dump != "" || p.DumpInArchive != "" || p.WordPress {
		return errors.New("a dump needs a database")
	}
	return nil
}
