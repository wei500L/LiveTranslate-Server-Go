// Package sqlitereader is a minimal, dependency-free READ-ONLY SQLite file
// parser. It exists so the one-shot import tool needs no CGO and no third
// party driver — the server's build stays verifiable offline.
//
// Scope (enough for the Python service's schema, nothing more):
//   - rollback-journal databases only: a non-empty -wal sidecar is REFUSED
//     (its committed data would be invisible to a raw read — make a copy
//     of the database after the service is stopped, or checkpoint first);
//   - table B-trees (rowid tables — every Python-era table is one);
//   - the full record format incl. overflow pages (long transcripts);
//   - NULL / int / float / text / blob values, UTF-8 text.
//
// Freelist pages, index B-trees, WAL mode databases and TEXT encodings other
// than UTF-8 are out of scope and rejected where detectable.
package sqlitereader

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// Value is one decoded cell value.
type Value struct {
	Type ValueType
	Int  int64
	Real float64
	Text string
	Blob []byte
}

type ValueType int

const (
	TypeNull ValueType = iota
	TypeInt
	TypeReal
	TypeText
	TypeBlob
)

// IsNull reports a NULL cell.
func (v Value) IsNull() bool { return v.Type == TypeNull }

// AsString renders TEXT/INT/REAL values ("" for NULL/BLOB).
func (v Value) AsString() string {
	switch v.Type {
	case TypeText:
		return v.Text
	case TypeInt:
		return fmt.Sprintf("%d", v.Int)
	case TypeReal:
		return fmt.Sprintf("%v", v.Real)
	default:
		return ""
	}
}

// AsInt extracts an integer (TEXT cells holding digits parse too — the
// Python schema has no such columns, but defensive is cheaper than wrong).
func (v Value) AsInt() (int64, error) {
	switch v.Type {
	case TypeInt:
		return v.Int, nil
	case TypeText:
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(v.Text), "%d", &n); err == nil {
			return n, nil
		}
		return 0, fmt.Errorf("text %q is not an integer", v.Text)
	default:
		return 0, fmt.Errorf("value is not an integer")
	}
}

// AsTime parses SQLAlchemy's SQLite datetime strings:
// "2006-01-02 15:04:05.635064+00:00" (space separator; offset optional).
func (v Value) AsTime() (time.Time, error) {
	raw := strings.TrimSpace(v.AsString())
	if raw == "" {
		return time.Time{}, errors.New("empty datetime")
	}
	normalized := strings.Replace(raw, "T", " ", 1)
	layouts := []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, normalized); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable datetime %q", raw)
}

// Row is one decoded table row keyed by column name.
type Row struct {
	RowID   int64
	Columns []string
	Values  []Value
}

// Col returns the value of a column (ok=false when absent).
func (r Row) Col(name string) (Value, bool) {
	for i, c := range r.Columns {
		if c == name {
			return r.Values[i], true
		}
	}
	return Value{}, false
}

// DB is an opened SQLite file.
type DB struct {
	data         []byte
	pageSize     int
	reserved     int
	usable       int
	tables       map[string]tableInfo
	textEncoding int // 1 = UTF-8 (only supported)
}

type tableInfo struct {
	rootPage int
	columns  []string
}

const headerMagic = "SQLite format 3\x00"

// Open reads the whole file into memory (import-time tool; source files are
// small) and parses the schema. The source is never written to.
func Open(path string) (*DB, error) {
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		return nil, errors.New("a non-empty -wal sidecar exists: stop the source service (or checkpoint/copy the database) so all committed data is in the main file")
	}
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if fi.Size() < 512 {
		return nil, errors.New("file too small to be a SQLite database")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if string(data[:16]) != headerMagic {
		return nil, errors.New("not a SQLite 3 database (bad magic)")
	}
	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return nil, fmt.Errorf("invalid page size %d", pageSize)
	}
	if len(data)%pageSize != 0 {
		return nil, fmt.Errorf("file size %d is not a multiple of the page size %d — truncated?", len(data), pageSize)
	}
	// Read version-valid-for + write version: only legacy journal files.
	if data[19] > 2 {
		return nil, fmt.Errorf("unsupported write version %d (only rollback-journal databases are supported)", data[19])
	}
	db := &DB{
		data:         data,
		pageSize:     pageSize,
		reserved:     int(data[20]),
		textEncoding: int(binary.BigEndian.Uint32(data[56:60])),
	}
	db.usable = pageSize - db.reserved
	if db.textEncoding != 0 && db.textEncoding != 1 {
		return nil, fmt.Errorf("unsupported text encoding %d (only UTF-8)", db.textEncoding)
	}
	if err := db.loadSchema(); err != nil {
		return nil, err
	}
	return db, nil
}

// Tables lists the user tables found in the schema.
func (db *DB) Tables() []string {
	var out []string
	for name := range db.tables {
		out = append(out, name)
	}
	return out
}

// HasTable reports whether the schema contains the table.
func (db *DB) HasTable(name string) bool {
	_, ok := db.tables[name]
	return ok
}

// loadSchema walks sqlite_master (page 1) and records each table's root
// page + column names (parsed from the CREATE TABLE statement — quoting
// follows SQLite's simple identifier rules, which the SQLAlchemy-generated
// schema never exceeds).
func (db *DB) loadSchema() error {
	db.tables = map[string]tableInfo{}
	rows, err := db.readTableBTree(1, []string{"type", "name", "tbl_name", "rootpage", "sql"})
	if err != nil {
		return fmt.Errorf("sqlite_master: %w", err)
	}
	for _, row := range rows {
		typ := row.Values[0].AsString()
		if typ != "table" {
			continue
		}
		name := row.Values[1].AsString()
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		root, err := row.Values[3].AsInt()
		if err != nil {
			return fmt.Errorf("table %s: rootpage: %w", name, err)
		}
		cols := parseCreateColumns(row.Values[4].AsString())
		db.tables[name] = tableInfo{rootPage: int(root), columns: cols}
	}
	return nil
}

// parseCreateColumns extracts the bare column names from a CREATE TABLE
// statement. Handles quoted identifiers and trailing table constraints
// (PRIMARY KEY (...), UNIQUE (...), CONSTRAINT ..., FOREIGN KEY ...) which
// must not be mistaken for columns.
func parseCreateColumns(sql string) []string {
	open := strings.Index(sql, "(")
	if open < 0 {
		return nil
	}
	// Cut at the matching close paren.
	depth := 0
	end := -1
	for i := open; i < len(sql); i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
				i = len(sql)
			}
		}
	}
	if end < 0 {
		return nil
	}
	body := sql[open+1 : end]

	// Split on top-level commas.
	var parts []string
	depth = 0
	inQuote := byte(0)
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inQuote = c
		case '[':
			inQuote = ']'
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, body[start:])

	constraintStarters := map[string]bool{
		"primary": true, "unique": true, "check": true, "foreign": true, "constraint": true,
	}
	var cols []string
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		// Unquote a quoted identifier.
		name := p
		if len(p) >= 2 && (p[0] == '"' || p[0] == '`' || p[0] == '[') {
			q := p[0]
			closer := map[byte]byte{'"': '"', '`': '`', '[': ']'}[q]
			if end := strings.IndexByte(p[1:], closer); end >= 0 {
				name = p[1 : 1+end]
			}
		} else {
			// First whitespace-delimited token.
			for i := 0; i < len(p); i++ {
				if p[i] == ' ' || p[i] == '\t' || p[i] == '\n' || p[i] == '\r' {
					name = p[:i]
					break
				}
			}
		}
		if constraintStarters[strings.ToLower(name)] {
			continue
		}
		cols = append(cols, strings.ToLower(name))
	}
	return cols
}

// ReadTable returns every row of a user table, in rowid order.
func (db *DB) ReadTable(name string) ([]Row, error) {
	info, ok := db.tables[name]
	if !ok {
		return nil, fmt.Errorf("no such table: %s", name)
	}
	return db.readTableBTree(info.rootPage, info.columns)
}

// --- B-tree walk ------------------------------------------------------------------

func (db *DB) page(n int) ([]byte, error) {
	if n < 1 {
		return nil, fmt.Errorf("invalid page number %d", n)
	}
	off := (n - 1) * db.pageSize
	if off+db.pageSize > len(db.data) {
		return nil, fmt.Errorf("page %d beyond end of file", n)
	}
	return db.data[off : off+db.pageSize], nil
}

func (db *DB) readTableBTree(root int, columns []string) ([]Row, error) {
	var rows []Row
	seen := map[int]bool{}
	var walk func(pageNo int) error
	walk = func(pageNo int) error {
		if seen[pageNo] {
			return fmt.Errorf("cycle detected at page %d (corrupt file?)", pageNo)
		}
		seen[pageNo] = true
		p, err := db.page(pageNo)
		if err != nil {
			return err
		}
		// Page 1 carries the 100-byte header before the b-tree page header.
		headerOff := 0
		if pageNo == 1 {
			headerOff = 100
		}
		pageType := p[headerOff]
		nCells := int(binary.BigEndian.Uint16(p[headerOff+3 : headerOff+5]))
		switch pageType {
		case 0x05: // interior table page
			cellPtrStart := headerOff + 12
			for i := 0; i < nCells; i++ {
				ptr := int(binary.BigEndian.Uint16(p[cellPtrStart+2*i : cellPtrStart+2*i+2]))
				if ptr+4 > len(p) {
					return fmt.Errorf("interior cell pointer out of range")
				}
				child := int(binary.BigEndian.Uint32(p[ptr : ptr+4]))
				if err := walk(child); err != nil {
					return err
				}
			}
			// Right-most pointer.
			right := int(binary.BigEndian.Uint32(p[headerOff+8 : headerOff+12]))
			return walk(right)
		case 0x0d: // leaf table page
			cellPtrStart := headerOff + 8
			for i := 0; i < nCells; i++ {
				ptr := int(binary.BigEndian.Uint16(p[cellPtrStart+2*i : cellPtrStart+2*i+2]))
				if ptr >= len(p) {
					return fmt.Errorf("leaf cell pointer out of range")
				}
				row, err := db.readLeafCell(p, ptr, columns)
				if err != nil {
					return fmt.Errorf("page %d cell %d: %w", pageNo, i, err)
				}
				rows = append(rows, row)
			}
			return nil
		default:
			return fmt.Errorf("page %d: unexpected b-tree page type 0x%02x (index pages are not supported)", pageNo, pageType)
		}
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return rows, nil
}

// readLeafCell decodes one table-leaf cell at offset `ptr`, following the
// overflow chain when the payload does not fit locally.
func (db *DB) readLeafCell(p []byte, ptr int, columns []string) (Row, error) {
	payloadLen, n1 := uvarint(p[ptr:])
	rowID, n2 := uvarint(p[ptr+n1:])
	dataStart := ptr + n1 + n2
	payloadLenInt := int(payloadLen)

	// Overflow computation (SQLite §B-tree Pages).
	usable := db.usable
	maxLocal := usable - 35
	if payloadLenInt <= maxLocal {
		if dataStart+payloadLenInt > len(p) {
			return Row{}, fmt.Errorf("payload overruns page")
		}
		return decodeRecord(p[dataStart:dataStart+payloadLenInt], int(rowID), columns)
	}
	minLocal := (usable-12)*32/255 - 23
	local := minLocal + (payloadLenInt-minLocal)%(usable-4)
	if local > maxLocal {
		local = minLocal
	}
	if dataStart+local+4 > len(p) {
		return Row{}, fmt.Errorf("overflow cell overruns page")
	}
	payload := make([]byte, 0, payloadLenInt)
	payload = append(payload, p[dataStart:dataStart+local]...)
	next := int(binary.BigEndian.Uint32(p[dataStart+local : dataStart+local+4]))
	remaining := payloadLenInt - local
	for remaining > 0 {
		if next == 0 {
			return Row{}, fmt.Errorf("overflow chain ends early")
		}
		op, err := db.page(next)
		if err != nil {
			return Row{}, err
		}
		chunk := usable - 4
		if chunk > remaining {
			chunk = remaining
		}
		payload = append(payload, op[4:4+chunk]...)
		remaining -= chunk
		next = int(binary.BigEndian.Uint32(op[0:4]))
	}
	return decodeRecord(payload, int(rowID), columns)
}

// uvarint decodes SQLite's big-endian varint (up to 9 bytes).
func uvarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 8 && i < len(b); i++ {
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	if len(b) < 9 {
		return v, len(b)
	}
	v = v<<8 | uint64(b[8])
	return v, 9
}

// decodeRecord decodes the SQLite record format: header of serial types
// followed by the value bytes.
func decodeRecord(rec []byte, rowID int, columns []string) (Row, error) {
	headerLen, hn := uvarint(rec)
	if int(headerLen) > len(rec) || hn > len(rec) {
		return Row{}, fmt.Errorf("record header overruns payload")
	}
	var serials []uint64
	pos := hn
	for pos < int(headerLen) {
		st, n := uvarint(rec[pos:])
		if n == 0 {
			break
		}
		serials = append(serials, st)
		pos += n
	}
	values := make([]Value, 0, len(serials))
	vpos := int(headerLen)
	for _, st := range serials {
		v, size, err := decodeSerial(rec, vpos, st)
		if err != nil {
			return Row{}, err
		}
		values = append(values, v)
		vpos += size
	}
	// Pad/trim to the column list.
	cols := columns
	if len(cols) == 0 {
		cols = make([]string, len(values))
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
		}
	}
	return Row{RowID: int64(rowID), Columns: cols, Values: values}, nil
}

func decodeSerial(rec []byte, pos int, serial uint64) (Value, int, error) {
	need := func(n int) error {
		if pos+n > len(rec) {
			return fmt.Errorf("value at %d overruns record", pos)
		}
		return nil
	}
	switch {
	case serial == 0:
		return Value{Type: TypeNull}, 0, nil
	case serial >= 1 && serial <= 6:
		sizes := map[uint64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}
		n := sizes[serial]
		if err := need(n); err != nil {
			return Value{}, 0, err
		}
		var v int64
		for i := 0; i < n; i++ {
			v = v<<8 | int64(rec[pos+i])
		}
		// Sign-extend.
		shift := uint(64 - 8*n)
		v = v << shift >> shift
		return Value{Type: TypeInt, Int: v}, n, nil
	case serial == 7:
		if err := need(8); err != nil {
			return Value{}, 0, err
		}
		bits := binary.BigEndian.Uint64(rec[pos : pos+8])
		return Value{Type: TypeReal, Real: math.Float64frombits(bits)}, 8, nil
	case serial == 8:
		return Value{Type: TypeInt, Int: 0}, 0, nil
	case serial == 9:
		return Value{Type: TypeInt, Int: 1}, 0, nil
	case serial >= 12 && serial%2 == 0:
		n := int((serial - 12) / 2)
		if err := need(n); err != nil {
			return Value{}, 0, err
		}
		blob := make([]byte, n)
		copy(blob, rec[pos:pos+n])
		return Value{Type: TypeBlob, Blob: blob}, n, nil
	case serial >= 13:
		n := int((serial - 13) / 2)
		if err := need(n); err != nil {
			return Value{}, 0, err
		}
		return Value{Type: TypeText, Text: string(rec[pos : pos+n])}, n, nil
	default:
		return Value{}, 0, fmt.Errorf("unsupported serial type %d", serial)
	}
}
