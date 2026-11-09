package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

// KindImportSite fills a website (and its database) the panel made for a website archive:
// it applies the server, then has the agent unpack the files and load the dump.
const KindImportSite = "import.site"

type ImportSiteParams struct {
	Server        string `json:"server"`
	SiteID        string `json:"site_id"`
	Archive       string `json:"archive"`
	Root          string `json:"root"`
	DatabaseID    string `json:"database_id,omitempty"`
	Dump          string `json:"dump,omitempty"`
	DumpInArchive string `json:"dump_in_archive,omitempty"`
	WordPress     bool   `json:"wordpress,omitempty"`
	// Report holds what the panel already knows.
	Report ImportReport `json:"report"`
}

func (r *Runner) QueueImportSite(ctx context.Context, p ImportSiteParams, by *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindImportSite, ServerID: &p.Server, Params: p, CreatedBy: by, MaxAttempts: 1})
}

func (r *Runner) importSite(ctx context.Context, t store.Task) (any, error) {
	var p ImportSiteParams
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	rep := p.Report
	if _, err := r.Reconcile(ctx, p.Server); err != nil {
		return rep, fmt.Errorf("set up the website: %w", err)
	}
	site, err := r.store.GetSite(ctx, p.SiteID)
	if err != nil {
		return rep, err
	}
	if site.State != "active" {
		return rep, fmt.Errorf("the website %s could not be set up", site.Domain)
	}
	apply := agentproto.ImportSiteApplyParams{Archive: p.Archive, Root: p.Root, User: site.LinuxUser, Dump: p.Dump,
		DumpInArchive: p.DumpInArchive, WordPress: p.WordPress}
	if p.DatabaseID != "" {
		d, err := r.store.GetDatabase(ctx, p.DatabaseID)
		if err != nil {
			return rep, err
		}
		if d.State != "active" {
			return rep, fmt.Errorf("the database %s could not be set up", d.Name)
		}
		apply.Database = &agentproto.TransferDatabase{Engine: d.Engine, Name: d.Name, User: d.Username}
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Hour)
	defer cancel()
	raw, err := r.hub.Call(callCtx, p.Server, agentproto.OpImportSiteApply, apply)
	if err != nil {
		return rep, fmt.Errorf("copy the files and database: %w", err)
	}
	var res agentproto.ImportApplyResult
	json.Unmarshal(raw, &res)
	rep.Files, rep.Bytes, rep.Databases = res.Files, res.Bytes, res.Databases
	rep.NotImported = append(rep.NotImported, skippedNotes(res.Skipped)...)
	return rep, nil
}

// skippedNotes puts what the agent left out into report notes.
func skippedNotes(skipped []agentproto.ImportSkip) []ImportNote {
	var out []ImportNote
	for _, sk := range skipped {
		switch sk.Name {
		case "links_outside", "special_files", "outside_website", "open_basedir", "dumps":
			out = append(out, ImportNote{Code: sk.Name, Count: sk.Count})
		default:
			out = append(out, ImportNote{Code: "folder", Name: sk.Name, Count: sk.Count})
		}
	}
	return out
}
