package fileguard

import (
	"archive/zip"
	"encoding/binary"
)

// plainZIP accepts only a contiguous, single-disk ZIP32 representation. Reject
// executable prefixes, hidden gaps/trailers, local-only extra fields and ZIP64;
// those bytes would otherwise escape inspection of the parsed members.
func plainZIP(b []byte, z *zip.Reader) bool {
	u16 := func(at int) uint16 { return binary.LittleEndian.Uint16(b[at : at+2]) }
	u32 := func(at int) uint32 { return binary.LittleEndian.Uint32(b[at : at+4]) }
	cursor := 0
	offsets := make([]int, 0, len(z.File))
	for _, f := range z.File {
		if len(b)-cursor < 30 || u32(cursor) != 0x04034b50 {
			return false
		}
		offsets = append(offsets, cursor)
		nameLen, extraLen := int(u16(cursor+26)), int(u16(cursor+28))
		if extraLen != 0 || nameLen > len(b)-cursor-30 || string(b[cursor+30:cursor+30+nameLen]) != f.Name {
			return false
		}
		flags := u16(cursor + 6)
		if flags != f.Flags || flags & ^uint16(0x080e) != 0 || u16(cursor+8) != f.Method || (f.Method != zip.Store && f.Method != zip.Deflate) {
			return false
		}
		offset, err := f.DataOffset()
		if err != nil || offset != int64(cursor+30+nameLen) || f.CompressedSize64 > uint64(len(b))-uint64(offset) {
			return false
		}
		cursor = int(offset) + int(f.CompressedSize64)
		if flags&8 != 0 {
			if len(b)-cursor >= 4 && u32(cursor) == 0x08074b50 {
				cursor += 4
			}
			if len(b)-cursor < 12 || u32(cursor) != f.CRC32 || uint64(u32(cursor+4)) != f.CompressedSize64 || uint64(u32(cursor+8)) != f.UncompressedSize64 {
				return false
			}
			cursor += 12
		} else {
			start := offsets[len(offsets)-1]
			if u32(start+14) != f.CRC32 || uint64(u32(start+18)) != f.CompressedSize64 || uint64(u32(start+22)) != f.UncompressedSize64 {
				return false
			}
		}
	}
	central := cursor
	for i, f := range z.File {
		if len(b)-cursor < 46 || u32(cursor) != 0x02014b50 {
			return false
		}
		n, x, c := int(u16(cursor+28)), int(u16(cursor+30)), int(u16(cursor+32))
		if x != 0 || n+c > len(b)-cursor-46 || u16(cursor+34) != 0 || int(u32(cursor+42)) != offsets[i] {
			return false
		}
		if string(b[cursor+46:cursor+46+n]) != f.Name || string(b[cursor+46+n:cursor+46+n+c]) != f.Comment {
			return false
		}
		cursor += 46 + n + c
	}
	if len(b)-cursor < 22 || u32(cursor) != 0x06054b50 || u16(cursor+4) != 0 || u16(cursor+6) != 0 {
		return false
	}
	if int(u16(cursor+8)) != len(z.File) || int(u16(cursor+10)) != len(z.File) || int(u32(cursor+12)) != cursor-central || int(u32(cursor+16)) != central {
		return false
	}
	comment := int(u16(cursor + 20))
	return len(b)-cursor == 22+comment && string(b[cursor+22:]) == z.Comment
}
