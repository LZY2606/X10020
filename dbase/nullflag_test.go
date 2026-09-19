package dbase

import (
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// nullFlagTestColumns builds a table layout with five nullable varchar columns
// (10 null flag state bits, exceeding a single bitmap byte), one non-nullable
// varchar column in the middle and ordinary columns after the variable length
// columns to guard their offsets.
func nullFlagTestColumns(t *testing.T) []*Column {
	t.Helper()
	names := []string{"V1", "V2", "VN", "V3", "V4", "V5"}
	columns := make([]*Column, 0, len(names)+2)
	for _, name := range names {
		nullable := name != "VN"
		column, err := NewColumn(name, Varchar, 8, 0, nullable)
		if err != nil {
			t.Fatalf("failed to create column %s: %v", name, err)
		}
		columns = append(columns, column)
	}
	character, err := NewColumn("C1", Character, 12, 0, false)
	if err != nil {
		t.Fatalf("failed to create column C1: %v", err)
	}
	integer, err := NewColumn("I1", Integer, 0, 0, false)
	if err != nil {
		t.Fatalf("failed to create column I1: %v", err)
	}
	return append(columns, character, integer)
}

func newNullFlagTestTable(t *testing.T, columns []*Column) *File {
	t.Helper()
	file, err := NewTable(
		FoxProVar,
		&Config{
			Filename:   filepath.Join(t.TempDir(), "nullflag.dbf"),
			Converter:  NewDefaultConverter(charmap.Windows1252),
			TrimSpaces: true,
		},
		columns,
		64,
		nil,
	)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	return file
}

// TestNullFlagColumnLengthRounding verifies that the hidden _NullFlags column
// is sized in whole bytes, rounded up, so every state bit fits.
func TestNullFlagColumnLengthRounding(t *testing.T) {
	tests := []struct {
		name     string
		nullable int
		plain    int
		expected uint8
	}{
		{"one nullable column", 1, 0, 1},
		{"four nullable columns fill exactly one byte", 4, 0, 1},
		{"five nullable columns need two bytes", 5, 0, 2},
		{"eight nullable columns fill exactly two bytes", 8, 0, 2},
		{"nine nullable columns need three bytes", 9, 0, 3},
		{"eight plain columns fill exactly one byte", 0, 8, 1},
		{"nine plain columns need two bytes", 0, 9, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			columns := make([]*Column, 0, tt.nullable+tt.plain)
			for i := 0; i < tt.nullable+tt.plain; i++ {
				name := string(rune('A'+i/26)) + string(rune('A'+i%26))
				column, err := NewColumn(name, Varchar, 8, 0, i < tt.nullable)
				if err != nil {
					t.Fatalf("failed to create column: %v", err)
				}
				columns = append(columns, column)
			}
			file := newNullFlagTestTable(t, columns)
			defer file.Close()
			if file.nullFlagColumn == nil {
				t.Fatal("expected a _NullFlags column to be created")
			}
			if file.nullFlagColumn.Length != tt.expected {
				t.Errorf("expected _NullFlags length %d, got %d", tt.expected, file.nullFlagColumn.Length)
			}
		})
	}
}

// TestRowToBytesNullFlagMultiByte is the regression test for the panic caused
// by a single-byte null flag bitmap when five nullable varchar columns push
// the state bits into a second byte. It also pins the exact bitmap layout.
func TestRowToBytesNullFlagMultiByte(t *testing.T) {
	columns := make([]*Column, 0, 5)
	for _, name := range []string{"V1", "V2", "V3", "V4", "V5"} {
		column, err := NewColumn(name, Varchar, 8, 0, true)
		if err != nil {
			t.Fatalf("failed to create column %s: %v", name, err)
		}
		columns = append(columns, column)
	}
	file := newNullFlagTestTable(t, columns)
	defer file.Close()

	if file.nullFlagColumn == nil || file.nullFlagColumn.Length != 2 {
		t.Fatalf("expected a 2 byte _NullFlags column, got %+v", file.nullFlagColumn)
	}

	row, err := file.RowFromMap(map[string]interface{}{
		"V1": "AB",       // short value => length flag at bit 0
		"V2": nil,        // null => null flag at bit 3
		"V3": "",         // empty string => length flag at bit 4
		"V4": "12345678", // full length => no flags
		"V5": "CD",       // short value => length flag at bit 8 (second byte)
	})
	if err != nil {
		t.Fatalf("failed to convert map to row: %v", err)
	}

	data, err := row.ToBytes()
	if err != nil {
		t.Fatalf("failed to convert row to bytes: %v", err)
	}

	expectedLength := 1 + 5*8 + 2
	if len(data) != expectedLength {
		t.Fatalf("expected row data of %d bytes, got %d", expectedLength, len(data))
	}
	// The null flag bitmap is appended at the end of the row:
	// byte 0: bit 0 (V1 length), bit 3 (V2 null), bit 4 (V3 length) => 0x19
	// byte 1: bit 0 (V5 length, global bit 8) => 0x01
	if data[expectedLength-2] != 0x19 || data[expectedLength-1] != 0x01 {
		t.Errorf("unexpected null flag bitmap: got %08b %08b, expected 00011001 00000001",
			data[expectedLength-2], data[expectedLength-1])
	}
	// The full length value must be stored verbatim, without a length byte.
	if string(data[1+3*8:1+4*8]) != "12345678" {
		t.Errorf("full length field corrupted: got %q", string(data[1+3*8:1+4*8]))
	}
}

// TestVarcharNullFlagRoundTrip writes rows covering null, empty string, short
// and full length values interleaved across more than eight null flag state
// bits and verifies that every column, the deletion flag and the offsets of
// the ordinary columns survive the round trip.
func TestVarcharNullFlagRoundTrip(t *testing.T) {
	file := newNullFlagTestTable(t, nullFlagTestColumns(t))

	type expectation map[string]interface{}
	rows := []expectation{
		{"V1": "AB", "V2": nil, "VN": "xyz", "V3": "", "V4": "12345678", "V5": "Q", "C1": "hello", "I1": int32(42)},
		{"V1": nil, "V2": nil, "VN": "", "V3": nil, "V4": nil, "V5": nil, "C1": "world", "I1": int32(-7)},
		{"V1": "AAAAAAAA", "V2": "BBBBBBBB", "VN": "CCCCCCCC", "V3": "DDDDDDDD", "V4": "EEEEEEEE", "V5": "FFFFFFFF", "C1": "full", "I1": int32(0)},
		{"V1": "", "V2": "", "VN": "", "V3": "", "V4": "", "V5": "", "C1": "empty", "I1": int32(1)},
	}
	deleted := map[int]bool{3: true}

	for i, values := range rows {
		row, err := file.RowFromMap(values)
		if err != nil {
			t.Fatalf("row %d: failed to convert map to row: %v", i, err)
		}
		row.Deleted = deleted[i]
		if err := row.Add(); err != nil {
			t.Fatalf("row %d: failed to append row: %v", i, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("failed to close table: %v", err)
	}

	reopened, err := OpenTable(&Config{
		Filename:   file.config.Filename,
		Converter:  NewDefaultConverter(charmap.Windows1252),
		TrimSpaces: true,
	})
	if err != nil {
		t.Fatalf("failed to reopen table: %v", err)
	}
	defer reopened.Close()

	// nullValue is what a null varchar field reads back as.
	nullValue := []byte{}
	expected := []expectation{
		{"V1": "AB", "V2": nullValue, "VN": "xyz", "V3": "", "V4": "12345678", "V5": "Q", "C1": "hello", "I1": int32(42)},
		{"V1": nullValue, "V2": nullValue, "VN": "", "V3": nullValue, "V4": nullValue, "V5": nullValue, "C1": "world", "I1": int32(-7)},
		{"V1": "AAAAAAAA", "V2": "BBBBBBBB", "VN": "CCCCCCCC", "V3": "DDDDDDDD", "V4": "EEEEEEEE", "V5": "FFFFFFFF", "C1": "full", "I1": int32(0)},
		{"V1": "", "V2": "", "VN": "", "V3": "", "V4": "", "V5": "", "C1": "empty", "I1": int32(1)},
	}

	for i, want := range expected {
		row, err := reopened.Row()
		if err != nil {
			t.Fatalf("row %d: failed to read row: %v", i, err)
		}
		if row.Deleted != deleted[i] {
			t.Errorf("row %d: expected deleted=%v, got %v", i, deleted[i], row.Deleted)
		}
		for name, value := range want {
			field := row.FieldByName(name)
			if field == nil {
				t.Fatalf("row %d: field %s not found", i, name)
			}
			if !reflect.DeepEqual(field.GetValue(), value) {
				t.Errorf("row %d field %s: expected %#v, got %#v", i, name, value, field.GetValue())
			}
		}
		reopened.Skip(1)
	}

	// The ordinary columns after the variable length columns must start at
	// fixed offsets and the deletion flag must be the first byte of the row.
	if err := reopened.GoTo(0); err != nil {
		t.Fatalf("failed to seek to first row: %v", err)
	}
	raw, err := reopened.ReadRow(0)
	if err != nil {
		t.Fatalf("failed to read raw row: %v", err)
	}
	if raw[0] != byte(Active) {
		t.Errorf("expected active deletion flag 0x20, got 0x%02x", raw[0])
	}
	// Offset of C1: 1 (deletion flag) + 6 varchar columns of 8 bytes.
	if string(raw[49:49+5]) != "hello" {
		t.Errorf("ordinary column C1 at wrong offset: got %q", string(raw[49:49+12]))
	}
	raw, err = reopened.ReadRow(3)
	if err != nil {
		t.Fatalf("failed to read raw row 3: %v", err)
	}
	if raw[0] != byte(Deleted) {
		t.Errorf("expected deleted flag 0x2a, got 0x%02x", raw[0])
	}
}

// TestVarcharEmptyStringVsNull pins the distinction between an empty string
// and a null value in a nullable varchar column.
func TestVarcharEmptyStringVsNull(t *testing.T) {
	column, err := NewColumn("V1", Varchar, 8, 0, true)
	if err != nil {
		t.Fatalf("failed to create column: %v", err)
	}
	file := newNullFlagTestTable(t, []*Column{column})

	values := []interface{}{"", nil, "x", "12345678"}
	for i, value := range values {
		row, err := file.RowFromMap(map[string]interface{}{"V1": value})
		if err != nil {
			t.Fatalf("row %d: failed to convert map to row: %v", i, err)
		}
		if err := row.Add(); err != nil {
			t.Fatalf("row %d: failed to append row: %v", i, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("failed to close table: %v", err)
	}

	reopened, err := OpenTable(&Config{
		Filename:   file.config.Filename,
		Converter:  NewDefaultConverter(charmap.Windows1252),
		TrimSpaces: true,
	})
	if err != nil {
		t.Fatalf("failed to reopen table: %v", err)
	}
	defer reopened.Close()

	expected := []interface{}{"", []byte{}, "x", "12345678"}
	for i, want := range expected {
		row, err := reopened.Row()
		if err != nil {
			t.Fatalf("row %d: failed to read row: %v", i, err)
		}
		if got := row.FieldByName("V1").GetValue(); !reflect.DeepEqual(got, want) {
			t.Errorf("row %d: expected %#v, got %#v", i, want, got)
		}
		reopened.Skip(1)
	}
}

// TestGetNthBitBoundary verifies that reading the bit directly past the end of
// a bitmap returns false instead of panicking.
func TestGetNthBitBoundary(t *testing.T) {
	if getNthBit([]byte{0xFF}, 8) {
		t.Error("expected bit past the end of a one byte bitmap to be false")
	}
	if getNthBit([]byte{0xFF, 0xFF}, 16) {
		t.Error("expected bit past the end of a two byte bitmap to be false")
	}
	if !getNthBit([]byte{0x00, 0x80}, 15) {
		t.Error("expected last bit of a two byte bitmap to be readable")
	}
}
