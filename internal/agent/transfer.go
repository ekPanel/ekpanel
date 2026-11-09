package agent

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"ekpanel/internal/agentproto"
)

// Moving a site to another server (see agentproto.OpTransferExport). The data is gzip of
// a sequence of sections. A section is a JSON header (uint32 length, then the JSON) followed
// by data frames (uint32 length, then the bytes) ended by an empty frame. Sections:
//
//	{"kind":"db","engine":"mariadb","name":"shop"}   a plain SQL dump
//	{"kind":"cert","issuer":"…","domain":"…","file":"…"}  a file of Caddy's certificate storage
//	{"kind":"home","user":"shop_1234"}               a tar of /home/<user> (owners and ACLs)
//	{"kind":"end","bytes":1234}                      the number of data bytes before it

const (
	transferMagic   = "EKPMOVE1"
	maxSectionJSON  = 4 << 10
	maxFrame        = 1 << 20
	transferTimeout = 24 * time.Hour
)

var issuerRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

type sectionHeader struct {
	Kind   string `json:"kind"`
	Engine string `json:"engine,omitempty"`
	Name   string `json:"name,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	Domain string `json:"domain,omitempty"`
	File   string `json:"file,omitempty"`
	User   string `json:"user,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// ---- Framing ----

type frameWriter struct {
	w     io.Writer
	bytes int64
}

func (f *frameWriter) section(h sectionHeader) error {
	b, _ := json.Marshal(h)
	if err := binary.Write(f.w, binary.BigEndian, uint32(len(b))); err != nil {
		return err
	}
	_, err := f.w.Write(b)
	return err
}

// Write writes one data frame (in pieces of at most maxFrame).
func (f *frameWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := min(len(p), maxFrame)
		if err := binary.Write(f.w, binary.BigEndian, uint32(c)); err != nil {
			return n, err
		}
		if _, err := f.w.Write(p[:c]); err != nil {
			return n, err
		}
		n += c
		f.bytes += int64(c)
		p = p[c:]
	}
	return n, nil
}

func (f *frameWriter) endSection() error { return binary.Write(f.w, binary.BigEndian, uint32(0)) }

type frameReader struct {
	r    *bufio.Reader
	left uint32 // bytes left in the current frame
	done bool   // the current section's empty frame was read
}

func (f *frameReader) next() (sectionHeader, error) {
	var h sectionHeader
	var n uint32
	if err := binary.Read(f.r, binary.BigEndian, &n); err != nil {
		return h, err
	}
	if n == 0 || n > maxSectionJSON {
		return h, errors.New("bad section header")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(f.r, b); err != nil {
		return h, err
	}
	f.left, f.done = 0, false
	return h, json.Unmarshal(b, &h)
}

// Read reads the current section's data; io.EOF at its end.
func (f *frameReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, io.EOF
	}
	for f.left == 0 {
		if err := binary.Read(f.r, binary.BigEndian, &f.left); err != nil {
			return 0, err
		}
		if f.left == 0 {
			f.done = true
			return 0, io.EOF
		}
		if f.left > maxFrame {
			return 0, errors.New("bad frame")
		}
	}
	if uint32(len(p)) > f.left {
		p = p[:f.left]
	}
	n, err := f.r.Read(p)
	f.left -= uint32(n)
	return n, err
}

// ---- Streams ----

// transfers are the export and import streams of one connection to the panel.
type transfers struct {
	sys  *system
	send func(agentproto.Envelope) error

	mu      sync.Mutex
	exports map[string]*exportStream
	imports map[string]*importStream
}

type exportStream struct {
	credit chan struct{} // one token per chunk the panel allows
	done   chan struct{} // closed when the panel stops the stream
}

type importStream struct {
	data   chan []byte
	cancel context.CancelFunc
}

func newTransfers(sys *system, send func(agentproto.Envelope) error) *transfers {
	return &transfers{sys: sys, send: send, exports: map[string]*exportStream{}, imports: map[string]*importStream{}}
}

func (t *transfers) has(stream string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, e := t.exports[stream]
	_, i := t.imports[stream]
	return e || i
}

// credit lets an export send n more chunks.
func (t *transfers) credit(env agentproto.Envelope) {
	t.mu.Lock()
	ex := t.exports[env.Stream]
	t.mu.Unlock()
	if ex == nil {
		return
	}
	for range min(env.Credit, agentproto.TransferWindow) {
		select {
		case ex.credit <- struct{}{}:
		default:
		}
	}
}

// input takes a chunk for an import. More than the window means the panel ignored flow
// control; the import fails rather than holding up the connection.
func (t *transfers) input(env agentproto.Envelope) {
	// Under the lock, so close cannot close the channel in between (the send never waits).
	t.mu.Lock()
	defer t.mu.Unlock()
	im := t.imports[env.Stream]
	if im == nil {
		return
	}
	select {
	case im.data <- env.Data:
	default:
		im.cancel()
	}
}

// close handles the panel closing a stream: "eof" ends an import's input; anything else
// stops the stream.
func (t *transfers) close(env agentproto.Envelope) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if im := t.imports[env.Stream]; im != nil {
		delete(t.imports, env.Stream)
		if env.Reason == "eof" {
			close(im.data)
		} else {
			im.cancel()
		}
	}
	if ex := t.exports[env.Stream]; ex != nil {
		delete(t.exports, env.Stream)
		close(ex.done)
	}
}

func (t *transfers) closeAll() {
	t.mu.Lock()
	ids := make([]string, 0, len(t.exports)+len(t.imports))
	for id := range t.exports {
		ids = append(ids, id)
	}
	for id := range t.imports {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	for _, id := range ids {
		t.close(agentproto.Envelope{Stream: id, Reason: "disconnected"})
	}
}

func (t *transfers) end(stream, reason string, result any) {
	env := agentproto.Envelope{Type: agentproto.TypeStreamClose, Stream: stream, Reason: reason}
	if result != nil {
		env.Result, _ = json.Marshal(result)
	}
	t.send(env)
}

// creditWriter sends chunks to the panel, each only when it has credit.
type creditWriter struct {
	t      *transfers
	stream string
	ex     *exportStream
	buf    []byte
}

func (w *creditWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.buf = append(w.buf, p...)
	for len(w.buf) >= agentproto.MaxStreamChunk {
		if err := w.flush(agentproto.MaxStreamChunk); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func (w *creditWriter) flush(n int) error {
	select {
	case <-w.ex.credit:
	case <-w.ex.done:
		return errors.New("the panel stopped the transfer")
	}
	chunk := append([]byte(nil), w.buf[:n]...)
	w.buf = w.buf[n:]
	return w.t.send(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: w.stream, Data: chunk})
}

func (w *creditWriter) Close() error {
	if len(w.buf) == 0 {
		return nil
	}
	return w.flush(len(w.buf))
}

// startExport begins sending; the stream ends with reason "done" and a TransferResult.
func (t *transfers) startExport(ctx context.Context, p agentproto.TransferExportParams) *agentproto.OpError {
	if err := p.Validate(); err != nil {
		return opError(agentproto.ErrInvalidParams, "%v", err)
	}
	if p.User != "" && !userRE.MatchString(p.User) {
		return opError(agentproto.ErrInvalidParams, "invalid user")
	}
	ex := &exportStream{credit: make(chan struct{}, agentproto.TransferWindow), done: make(chan struct{})}
	t.mu.Lock()
	t.exports[p.Stream] = ex
	t.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transferTimeout)
		defer cancel()
		cw := &creditWriter{t: t, stream: p.Stream, ex: ex}
		res, err := t.sys.export(ctx, cw, p)
		if err == nil {
			err = cw.Close()
		}
		t.mu.Lock()
		if t.exports[p.Stream] == ex {
			delete(t.exports, p.Stream)
		}
		t.mu.Unlock()
		if err != nil {
			t.end(p.Stream, "error: "+err.Error(), nil)
			return
		}
		t.end(p.Stream, "done", res)
	}()
	return nil
}

// export writes the sections for p to w.
func (s *system) export(ctx context.Context, w io.Writer, p agentproto.TransferExportParams) (agentproto.TransferResult, error) {
	var res agentproto.TransferResult
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if _, err := io.WriteString(zw, transferMagic); err != nil {
		return res, err
	}
	fw := &frameWriter{w: zw}
	section := func(h sectionHeader, write func(io.Writer) error) error {
		if err := fw.section(h); err != nil {
			return err
		}
		if err := write(fw); err != nil {
			return err
		}
		return fw.endSection()
	}

	for _, d := range p.Databases {
		err := section(sectionHeader{Kind: "db", Engine: d.Engine, Name: d.Name}, func(w io.Writer) error {
			var stderr strings.Builder
			c := command{stdout: w, stderr: &stderr}
			if d.Engine == "mariadb" {
				// No --databases: the dump must not create or switch databases; the target
				// already has this one.
				c.name, c.args = "mysqldump", []string{"--single-transaction", "--routines", "--triggers", "--events",
					"--hex-blob", "--default-character-set=utf8mb4", d.Name}
			} else {
				c.name, c.args = "runuser", []string{"-u", "postgres", "--", "pg_dump", "--no-owner", "--no-privileges", "-Fp", d.Name}
			}
			if _, err := s.exec(ctx, c); err != nil {
				return fmt.Errorf("dump %s: %w: %s", d.Name, err, strings.TrimSpace(stderr.String()))
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		res.Databases++
	}

	for _, domain := range p.Domains {
		issuers, _ := os.ReadDir(s.path(caddyCertDir))
		for _, iss := range issuers {
			dir := filepath.Join(caddyCertDir, iss.Name(), domain)
			files, err := os.ReadDir(s.path(dir))
			if err != nil || !issuerRE.MatchString(iss.Name()) {
				continue
			}
			for _, f := range files {
				if !f.Type().IsRegular() || !strings.HasPrefix(f.Name(), domain+".") {
					continue
				}
				b, err := os.ReadFile(s.path(filepath.Join(dir, f.Name())))
				if err != nil {
					continue
				}
				err = section(sectionHeader{Kind: "cert", Issuer: iss.Name(), Domain: domain, File: f.Name()}, func(w io.Writer) error {
					_, err := w.Write(b)
					return err
				})
				if err != nil {
					return res, err
				}
				res.Certs++
			}
		}
	}

	if p.User != "" {
		err := section(sectionHeader{Kind: "home", User: p.User}, func(w io.Writer) error {
			var stderr strings.Builder
			// Owners by name (the account, root, caddy in ACLs): numbers differ between servers.
			_, err := s.exec(ctx, command{name: "tar", args: []string{"--acls", "--xattrs", "-C", s.path("/home"), "-cf", "-", p.User},
				stdout: w, stderr: &stderr})
			if err != nil {
				return fmt.Errorf("pack the files: %w: %s", err, strings.TrimSpace(stderr.String()))
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		res.Files = true
	}

	res.Bytes = fw.bytes
	if err := fw.section(sectionHeader{Kind: "end", Bytes: fw.bytes}); err != nil {
		return res, err
	}
	if err := fw.endSection(); err != nil {
		return res, err
	}
	return res, zw.Close()
}

// startImport begins receiving into the incoming folder; it grants the panel the first
// credit and confirms every chunk it has written.
func (t *transfers) startImport(ctx context.Context, p agentproto.TransferImportParams) *agentproto.OpError {
	if err := p.Validate(); err != nil {
		return opError(agentproto.ErrInvalidParams, "%v", err)
	}
	dir := t.sys.path(filepath.Join(agentproto.IncomingDir, p.Move))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return opError(agentproto.ErrFailed, "%v", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transferTimeout)
	im := &importStream{data: make(chan []byte, agentproto.TransferWindow+8), cancel: cancel}
	t.mu.Lock()
	t.imports[p.Stream] = im
	t.mu.Unlock()

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			select {
			case <-ctx.Done():
				pw.CloseWithError(errors.New("the transfer was stopped"))
				return
			case chunk, ok := <-im.data:
				if !ok {
					return
				}
				if _, err := pw.Write(chunk); err != nil {
					return
				}
				t.send(agentproto.Envelope{Type: agentproto.TypeStreamCredit, Stream: p.Stream, Credit: 1})
			}
		}
	}()
	go func() {
		defer cancel()
		res, err := t.sys.receive(pr, dir)
		pr.CloseWithError(errors.New("done")) // unblock the writer if we stopped early
		t.mu.Lock()
		delete(t.imports, p.Stream)
		t.mu.Unlock()
		if err != nil {
			os.RemoveAll(dir)
			t.end(p.Stream, "error: "+err.Error(), nil)
			return
		}
		t.end(p.Stream, "done", res)
	}()
	t.send(agentproto.Envelope{Type: agentproto.TypeStreamCredit, Stream: p.Stream, Credit: agentproto.TransferWindow})
	return nil
}

// receive reads the sections into dir: database dumps and the home tar as files, and
// certificates straight into Caddy's storage.
func (s *system) receive(r io.Reader, dir string) (agentproto.TransferResult, error) {
	var res agentproto.TransferResult
	zr, err := gzip.NewReader(r)
	if err != nil {
		return res, err
	}
	magic := make([]byte, len(transferMagic))
	if _, err := io.ReadFull(zr, magic); err != nil || string(magic) != transferMagic {
		return res, errors.New("this is not an ekPanel transfer")
	}
	fr := &frameReader{r: bufio.NewReaderSize(zr, 64<<10)}
	var counted int64
	countingCopy := func(dst io.Writer) error {
		n, err := io.Copy(dst, fr)
		counted += n
		return err
	}
	toFile := func(path string, mode os.FileMode) error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		if err := countingCopy(f); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	for {
		h, err := fr.next()
		if err != nil {
			return res, fmt.Errorf("the transfer ended early: %w", err)
		}
		switch h.Kind {
		case "db":
			if (h.Engine != "mariadb" && h.Engine != "postgresql") || !dbNameRE.MatchString(h.Name) {
				return res, errors.New("bad database section")
			}
			if err := toFile(filepath.Join(dir, "db-"+h.Engine+"-"+h.Name+".sql"), 0o600); err != nil {
				return res, err
			}
			res.Databases++
		case "cert":
			if !issuerRE.MatchString(h.Issuer) || !hostnameRE.MatchString(strings.TrimPrefix(h.Domain, "*.")) ||
				!strings.HasPrefix(h.File, h.Domain+".") || strings.ContainsAny(h.File, "/\\") ||
				!slices.Contains([]string{".crt", ".key", ".json"}, filepath.Ext(h.File)) {
				return res, errors.New("bad certificate section")
			}
			cdir := s.path(filepath.Join(caddyCertDir, h.Issuer, h.Domain))
			if err := os.MkdirAll(cdir, 0o700); err != nil {
				return res, err
			}
			if err := toFile(filepath.Join(cdir, h.File), 0o600); err != nil {
				return res, err
			}
			// Caddy runs as its own user and must read its storage, folders made here included.
			s.run(context.Background(), "chown", "-R", "caddy:caddy", s.path(caddyCertDir))
			res.Certs++
		case "home":
			if !userRE.MatchString(h.User) {
				return res, errors.New("bad home section")
			}
			if err := toFile(filepath.Join(dir, "home.tar"), 0o600); err != nil {
				return res, err
			}
			os.WriteFile(filepath.Join(dir, "user"), []byte(h.User), 0o600)
			res.Files = true
		case "end":
			if h.Bytes != counted {
				return res, fmt.Errorf("received %d bytes, the source sent %d", counted, h.Bytes)
			}
			io.Copy(io.Discard, fr)
			res.Bytes = counted
			return res, os.WriteFile(filepath.Join(dir, "complete"), nil, 0o600)
		default:
			return res, fmt.Errorf("unknown section %q", h.Kind)
		}
	}
}

// ---- Putting it in place ----

// transferFinish puts an imported site in place on this (target) server: the home folder's
// contents and the database dumps. The panel has reconciled the site first, so the account,
// the databases and their users exist.
func transferFinish(ctx context.Context, s *system, p agentproto.TransferFinishParams) (any, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	dir := s.path(filepath.Join(agentproto.IncomingDir, p.Move))
	if _, err := os.Stat(filepath.Join(dir, "complete")); err != nil {
		return nil, opError(agentproto.ErrFailed, "the copy of this move is not complete on this server")
	}
	var res agentproto.TransferFinishResult
	if p.User != "" {
		if !userRE.MatchString(p.User) {
			return nil, opError(agentproto.ErrInvalidParams, "invalid user")
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "user")); string(b) != p.User {
			return nil, opError(agentproto.ErrFailed, "the copied files belong to another account")
		}
		if err := s.unpackHome(ctx, filepath.Join(dir, "home.tar"), p.User); err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
	}
	for _, d := range p.Databases {
		if !dbNameRE.MatchString(d.Name) || !dbNameRE.MatchString(d.User) {
			return nil, opError(agentproto.ErrInvalidParams, "invalid database")
		}
		if err := s.loadDump(ctx, filepath.Join(dir, "db-"+d.Engine+"-"+d.Name+".sql"), d); err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
	}
	os.RemoveAll(dir)
	if p.Domain != "" && s.probe != nil {
		if status, _, err := s.probe(ctx, p.Domain); err == nil {
			res.Status = status
		}
	}
	return res, nil
}

// unpackHome replaces the site's files with the copied ones. Every name in the archive
// must be inside the account's folder. Owners and ACL entries are kept by name, so the
// account (made here under the same name), root and caddy map to this server's numbers.
func (s *system) unpackHome(ctx context.Context, archive, user string) error {
	list, err := s.run(ctx, "tar", "-tf", archive)
	if err != nil {
		return fmt.Errorf("read the copied files: %w", err)
	}
	for _, name := range strings.Split(strings.TrimSpace(string(list)), "\n") {
		name = strings.TrimSuffix(name, "/")
		if name != user && !strings.HasPrefix(name, user+"/") || strings.Contains(name, "/../") || strings.HasSuffix(name, "/..") {
			return fmt.Errorf("the copied files contain %q, outside the site's folder", name)
		}
	}
	home := s.path("/home/" + user)
	// The account was just made here with a fresh public_html; the copy replaces it.
	os.RemoveAll(filepath.Join(home, "public_html"))
	if _, err := s.run(ctx, "tar", "--acls", "--xattrs", "--same-owner", "--same-permissions", "-C", s.path("/home"), "-xf", archive); err != nil {
		return fmt.Errorf("unpack the files: %w", err)
	}
	return nil
}

// loadDump loads a plain SQL dump into an existing database as its owner.
func (s *system) loadDump(ctx context.Context, path string, d agentproto.TransferDatabase) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("the dump of %s was not copied", d.Name)
	}
	defer f.Close()
	var stderr strings.Builder
	c := command{stderr: &stderr}
	if d.Engine == "mariadb" {
		c.name, c.args, c.stdin = "mysql", []string{"--default-character-set=utf8mb4", d.Name}, f
	} else {
		c.name = "runuser"
		c.args = []string{"-u", "postgres", "--", "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", d.Name}
		c.stdin = io.MultiReader(strings.NewReader("SET ROLE "+d.User+";\n"), f)
	}
	if _, err := s.exec(ctx, c); err != nil {
		return fmt.Errorf("load the dump of %s: %w: %s", d.Name, err, lastLines([]byte(stderr.String()), 5))
	}
	return nil
}

func transferDiscard(s *system, p agentproto.TransferDiscardParams) (any, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	os.RemoveAll(s.path(filepath.Join(agentproto.IncomingDir, p.Move)))
	return struct{}{}, nil
}
