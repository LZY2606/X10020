package dbase

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// nullFlagColumns builds a layout with five nullable varchar columns (10 null
// flag bits), one non-nullable varbinary column (1 more bit) and a regular
// character column trailing the variable length block. The character column
// guards the offsets of the ordinary columns that follow the bitmap.
func nullFlagColumns(tb testing.TB) []*Column {
	tb.Helper()

	specs := []struct {
		name     string
		dataType DataType
		length   uint8
		nullable bool
	}{
		{"VC0", Varchar, 10, true},
		{"VC1", Varchar, 10, true},
		{"VC2", Varchar, 10, true},
		{"VC3", Varchar, 10, true},
		{"VC4", Varchar, 10, true},
		{"VB5", Varbinary, 8, false},
		{"NAMEC", Character, 12, false},
	}

	columns := make([]*Column, 0, len(specs))
	for _, spec := range specs {
		column, err := NewColumn(spec.name, spec.dataType, spec.length, 0, spec.nullable)
		if err != nil {
			tb.Fatalf("creating column %s failed: %v", spec.name, err)
		}
		columns = append(columns, column)
	}
	return columns
}

// newNullFlagTable creates a fresh table in a temporary directory. The
// directory is created relative to the working directory with an upper case
// name on purpose: the Create implementations upper case the whole path, so a
// table cannot be created below a lower case directory such as the one
// returned by testing.TB.TempDir.
func newNullFlagTable(tb testing.TB, columns []*Column) *File {
	tb.Helper()

	dir, err := os.MkdirTemp(".", "TESTDATA")
	if err != nil {
		tb.Fatalf("creating test directory failed: %v", err)
	}
	tb.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	file, err := NewTable(
		FoxProVar,
		&Config{
			Filename:   filepath.Join(dir, "NULLFLAG.DBF"),
			Converter:  NewDefaultConverter(charmap.Windows1250),
			TrimSpaces: true,
		},
		columns,
		0,
		nil,
	)
	if err != nil {
		tb.Fatalf("creating table failed: %v", err)
	}
	tb.Cleanup(func() {
		_ = file.Close()
	})
	return file
}

// nullFlagInterleavedValues returns a row where null, empty, short and full
// length values interleave across the variable length columns:
//
//	VC0: "short"      -> variable length bit
//	VC1: ""           -> empty string, variable length bit with length 0
//	VC2: nil          -> null bit
//	VC3: nil          -> null bit at bit position 7 (byte boundary crossing)
//	VC4: full length  -> no bits at all
//	VB5: {1,2,3}      -> variable length bit in the second bitmap byte
func nullFlagInterleavedValues() map[string]interface{} {
	return map[string]interface{}{
		"VC0":   "short",
		"VC1":   "",
		"VC2":   nil,
		"VC3":   nil,
		"VC4":   "0123456789",
		"VB5":   []byte{1, 2, 3},
		"NAMEC": "normal",
	}
}

// TestRowToBytesNullFlagMultiByte locks the layout of the raw row encoding for
// a table whose null flag bitmap spans more than one byte: five nullable
// varchar columns alone need ten state bits. Writing such a row used to panic
// because the bitmap was hardcoded to a single byte.
func TestRowToBytesNullFlagMultiByte(t *testing.T) {
	file := newNullFlagTable(t, nullFlagColumns(t))

	// 11 state bits (5 * (varlength + null) + 1 varlength) need two bytes.
	if file.nullFlagColumn == nil {
		t.Fatal("expected a null flag column for a table with variable length columns")
	}
	if file.nullFlagColumn.Length != 2 {
		t.Errorf("expected null flag column length 2, got %d", file.nullFlagColumn.Length)
	}

	row, err := file.RowFromMap(nullFlagInterleavedValues())
	if err != nil {
		t.Fatalf("building row failed: %v", err)
	}

	data, err := row.ToBytes()
	if err != nil {
		t.Fatalf("converting row to bytes failed: %v", err)
	}

	// The row length must cover the delete flag, all columns and the bitmap.
	wantRowLength := uint16(1 + 5*10 + 8 + 12 + 2)
	if file.header.RowLength != wantRowLength {
		t.Errorf("expected row length %d, got %d", wantRowLength, file.header.RowLength)
	}
	if len(data) != int(wantRowLength) {
		t.Fatalf("expected %d row bytes, got %d", wantRowLength, len(data))
	}

	// The delete flag stays an untouched active marker at byte 0.
	if Marker(data[0]) != Active {
		t.Errorf("expected active delete marker 0x20, got 0x%02x", data[0])
	}

	// The ordinary character column keeps its offset behind the variable
	// length block: 1 (delete flag) + 5*10 (varchar) + 8 (varbinary) = 59.
	name := string(data[59 : 59+12])
	if name != "normal      " {
		t.Errorf("expected space padded character column %q, got %q", "normal      ", name)
	}

	// The bitmap sits behind the last ordinary column at offset 71.
	// Bits: 0 (VC0 varlen), 2 (VC1 varlen), 5 (VC2 null), 7 (VC3 null) and
	// 10 (VB5 varlen, second byte).
	if data[71] != 0xA5 {
		t.Errorf("expected first null flag byte 0xA5, got 0x%02x", data[71])
	}
	if data[72] != 0x04 {
		t.Errorf("expected second null flag byte 0x04, got 0x%02x", data[72])
	}

	// A deleted row keeps the deleted marker while the bitmap stays intact.
	row.Deleted = true
	deleted, err := row.ToBytes()
	if err != nil {
		t.Fatalf("converting deleted row to bytes failed: %v", err)
	}
	if Marker(deleted[0]) != Deleted {
		t.Errorf("expected deleted marker 0x2A, got 0x%02x", deleted[0])
	}
	if deleted[71] != 0xA5 || deleted[72] != 0x04 {
		t.Errorf("expected unchanged null flag bytes 0xA5 0x04, got 0x%02x 0x%02x", deleted[71], deleted[72])
	}
}

// TestRowToBytesNullFlagByteBoundary locks the null bit placement when the bit
// pair of a nullable column crosses a byte boundary: with four nullable
// varchar columns the null bit of the fourth column is bit 7 of the bitmap.
// Setting "bit 8" of the first byte used to be a silent no-op, so the null
// state was lost on write.
func TestRowToBytesNullFlagByteBoundary(t *testing.T) {
	columns := make([]*Column, 0, 4)
	for _, name := range []string{"VC0", "VC1", "VC2", "VC3"} {
		column, err := NewColumn(name, Varchar, 10, 0, true)
		if err != nil {
			t.Fatalf("creating column %s failed: %v", name, err)
		}
		columns = append(columns, column)
	}
	file := newNullFlagTable(t, columns)

	if file.nullFlagColumn == nil || file.nullFlagColumn.Length != 1 {
		t.Fatalf("expected a one byte null flag column, got %+v", file.nullFlagColumn)
	}

	row, err := file.RowFromMap(map[string]interface{}{
		"VC0": "aaaaaaaaaa",
		"VC1": "bbbbbbbbbb",
		"VC2": "cccccccccc",
		"VC3": nil,
	})
	if err != nil {
		t.Fatalf("building row failed: %v", err)
	}

	data, err := row.ToBytes()
	if err != nil {
		t.Fatalf("converting row to bytes failed: %v", err)
	}

	// Only bit 7 (null bit of VC3) must be set.
	if data[41] != 0x80 {
		t.Errorf("expected null flag byte 0x80, got 0x%02x", data[41])
	}
}

// TestNullableVarcharRoundTrip writes rows with interleaved null, empty, short
// and full length values through the file and reads them back: every column
// must come back exactly as it went in.
func TestNullableVarcharRoundTrip(t *testing.T) {
	file := newNullFlagTable(t, nullFlagColumns(t))

	inputs := []map[string]interface{}{
		nullFlagInterleavedValues(),
		{
			"VC0":   nil,
			"VC1":   "aaaaaaaaaa",
			"VC2":   "",
			"VC3":   "abc",
			"VC4":   nil,
			"VB5":   []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48},
			"NAMEC": "second",
		},
		{
			"VC0":   "x",
			"VC1":   nil,
			"VC2":   "zz",
			"VC3":   "yyyyyyyyyy",
			"VC4":   "",
			"VB5":   nil,
			"NAMEC": "third",
		},
	}

	path := file.config.Filename
	for i, values := range inputs {
		row, err := file.RowFromMap(values)
		if err != nil {
			t.Fatalf("building row %d failed: %v", i, err)
		}
		if err := row.Add(); err != nil {
			t.Fatalf("writing row %d failed: %v", i, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing table failed: %v", err)
	}

	reopened, err := OpenTable(&Config{
		Filename:   path,
		Converter:  NewDefaultConverter(charmap.Windows1250),
		TrimSpaces: true,
	})
	if err != nil {
		t.Fatalf("reopening table failed: %v", err)
	}
	defer reopened.Close()

	if reopened.RowsCount() != uint32(len(inputs)) {
		t.Fatalf("expected %d rows, got %d", len(inputs), reopened.RowsCount())
	}

	// Null reads back as an empty []byte, everything else keeps its type.
	expected := []map[string]interface{}{
		{
			"VC0":   "short",
			"VC1":   "",
			"VC2":   []byte{},
			"VC3":   []byte{},
			"VC4":   "0123456789",
			"VB5":   []byte{1, 2, 3},
			"NAMEC": "normal",
		},
		{
			"VC0":   []byte{},
			"VC1":   "aaaaaaaaaa",
			"VC2":   "",
			"VC3":   "abc",
			"VC4":   []byte{},
			"VB5":   []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48},
			"NAMEC": "second",
		},
		{
			"VC0":   "x",
			"VC1":   []byte{},
			"VC2":   "zz",
			"VC3":   "yyyyyyyyyy",
			"VC4":   "",
			"VB5":   []byte{},
			"NAMEC": "third",
		},
	}

	for i, want := range expected {
		row, err := reopened.Next()
		if err != nil {
			t.Fatalf("reading row %d failed: %v", i, err)
		}
		if row.Deleted {
			t.Errorf("row %d: expected active row, got deleted", i)
		}
		for name, wantValue := range want {
			field := row.FieldByName(name)
			if field == nil {
				t.Fatalf("row %d: column %s not found", i, name)
			}
			got := field.GetValue()
			if !valuesEqual(got, wantValue) {
				t.Errorf("row %d column %s: expected %#v, got %#v", i, name, wantValue, got)
			}
		}
	}
}

func valuesEqual(got, want interface{}) bool {
	switch wantValue := want.(type) {
	case string:
		gotValue, ok := got.(string)
		return ok && gotValue == wantValue
	case []byte:
		gotValue, ok := got.([]byte)
		if !ok || len(gotValue) != len(wantValue) {
			return false
		}
		for i := range wantValue {
			if gotValue[i] != wantValue[i] {
				return false
			}
		}
		return true
	}
	return false
}
