package font

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const MaxCmapSegments = 20000

type SFNT struct {
	Data              []byte
	IsCFF, IsTrueType bool // only one can be true
	Tables            map[string][]byte

	// required
	Cmap *cmapTable
	Head *headTable
	Hhea *hheaTable
	Hmtx *hmtxTable
	Maxp *maxpTable
	Name *nameTable
	OS2  *os2Table
	Post *postTable

	// TrueType
	Glyf *glyfTable
	Loca *locaTable

	// CFF
	//CFF  *cffTable

	// optional
	Kern *kernTable
	//Gpos *gposTable
	//Gasp *gaspTable

}

func (sfnt *SFNT) GlyphIndex(r rune) uint16 {
	return sfnt.Cmap.Get(r)
}

func (sfnt *SFNT) GlyphName(glyphID uint16) string {
	return sfnt.Post.Get(glyphID)
}

func (sfnt *SFNT) GlyphContour(glyphID uint16) (*glyfContour, error) {
	if !sfnt.IsTrueType {
		return nil, fmt.Errorf("CFF not supported")
	}
	return sfnt.Glyf.Contour(glyphID, 0)
}

func (sfnt *SFNT) GlyphAdvance(glyphID uint16) uint16 {
	return sfnt.Hmtx.Advance(glyphID)
}

func (sfnt *SFNT) Kerning(left, right uint16) int16 {
	return sfnt.Kern.Get(left, right)
}

func ParseSFNT(b []byte) (*SFNT, error) {
	if len(b) < 12 || math.MaxInt32 < len(b) {
		return nil, ErrInvalidFontData
	}

	r := newBinaryReader(b)
	sfntVersion := r.ReadString(4)
	if sfntVersion != "OTTO" && binary.BigEndian.Uint32([]byte(sfntVersion)) != 0x00010000 {
		return nil, fmt.Errorf("bad SFNT version")
	}
	numTables := r.ReadUint16()
	_ = r.ReadUint16() // searchRange
	_ = r.ReadUint16() // entrySelector
	_ = r.ReadUint16() // rangeShift

	frontSize := 12 + 16*uint32(numTables) // can never exceed uint32 as numTables is uint16
	if uint32(len(b)) < frontSize {
		return nil, ErrInvalidFontData
	}

	var checksumAdjustment uint32
	tables := make(map[string][]byte, numTables)
	for i := 0; i < int(numTables); i++ {
		tag := r.ReadString(4)
		checksum := r.ReadUint32()
		offset := r.ReadUint32()
		length := r.ReadUint32()

		padding := (4 - length&3) & 3
		if uint32(len(b)) <= offset || uint32(len(b))-offset < length || uint32(len(b))-offset-length < padding {
			return nil, ErrInvalidFontData
		}

		if tag == "head" {
			if length < 12 {
				return nil, ErrInvalidFontData
			}

			// to check checksum for head table, replace the overal checksum with zero and reset it at the end
			checksumAdjustment = binary.BigEndian.Uint32(b[offset+8:])
			binary.BigEndian.PutUint32(b[offset+8:], 0x00000000)
		}
		if calcChecksum(b[offset:offset+length+padding]) != checksum {
			return nil, fmt.Errorf("%s: bad checksum", tag)
		}
		if tag == "head" {
			binary.BigEndian.PutUint32(b[offset+8:], checksumAdjustment)
		}
		tables[tag] = b[offset : offset+length : offset+length]
	}
	// TODO: check file checksum

	sfnt := &SFNT{}
	sfnt.Data = b
	sfnt.IsCFF = sfntVersion == "OTTO"
	sfnt.IsTrueType = binary.BigEndian.Uint32([]byte(sfntVersion)) == 0x00010000
	sfnt.Tables = tables

	requiredTables := []string{"cmap", "head", "hhea", "hmtx", "maxp", "name", "OS/2", "post"}
	if sfnt.IsTrueType {
		requiredTables = append(requiredTables, "glyf", "loca")
	}
	for _, requiredTable := range requiredTables {
		if _, ok := tables[requiredTable]; !ok {
			return nil, fmt.Errorf("%s: missing table", requiredTable)
		}
	}
	if sfnt.IsCFF {
		_, hasCFF := tables["CFF "]
		_, hasCFF2 := tables["CFF2"]
		if !hasCFF && !hasCFF2 {
			return nil, fmt.Errorf("CFF: missing table")
		} else if hasCFF && hasCFF2 {
			return nil, fmt.Errorf("CFF2: CFF table already exists")
		}
	}

	// maxp and hhea tables are required for other tables to be parse first
	if err := sfnt.parseHead(); err != nil {
		return nil, err
	} else if err := sfnt.parseMaxp(); err != nil {
		return nil, err
	} else if err := sfnt.parseHhea(); err != nil {
		return nil, err
	}
	if sfnt.IsTrueType {
		if err := sfnt.parseLoca(); err != nil {
			return nil, err
		}
	}

	tableNames := make([]string, len(tables))
	for tableName, _ := range tables {
		tableNames = append(tableNames, tableName)
	}
	sort.Strings(tableNames)
	for _, tableName := range tableNames {
		var err error
		switch tableName {
		//case "CFF ":
		//	err = sfnt.parseCFF()
		//case "CFF2":
		//	err = sfnt.parseCFF2()
		case "cmap":
			err = sfnt.parseCmap()
		case "glyf":
			err = sfnt.parseGlyf()
		case "hmtx":
			err = sfnt.parseHmtx()
		case "kern":
			err = sfnt.parseKern()
		case "name":
			err = sfnt.parseName()
		case "OS/2":
			err = sfnt.parseOS2()
		case "post":
			err = sfnt.parsePost()
		}
		if err != nil {
			return nil, err
		}
	}
	return sfnt, nil
}

////////////////////////////////////////////////////////////////

type cmapFormat0 struct {
	GlyphIdArray [256]uint8
}

func (subtable *cmapFormat0) Get(r rune) (uint16, bool) {
	if r < 0 || 256 <= r {
		return 0, false
	}
	return uint16(subtable.GlyphIdArray[r]), true
}

type cmapFormat4 struct {
	StartCode     []uint16
	EndCode       []uint16
	IdDelta       []int16
	IdRangeOffset []uint16
	GlyphIdArray  []uint16
}

func (subtable *cmapFormat4) Get(r rune) (uint16, bool) {
	if r < 0 || 65536 <= r {
		return 0, false
	}
	n := len(subtable.StartCode)
	for i := 0; i < n; i++ {
		if uint16(r) <= subtable.EndCode[i] && subtable.StartCode[i] <= uint16(r) {
			if subtable.IdRangeOffset[i] == 0 {
				// is modulo 65536 with the idDelta cast and addition overflow
				return uint16(subtable.IdDelta[i]) + uint16(r), true
			}
			// idRangeOffset/2  ->  offset value to index of words
			// r-startCode  ->  difference of rune with startCode
			// -(n-1)  ->  subtract offset from the current idRangeOffset item
			index := int(subtable.IdRangeOffset[i]/2) + int(uint16(r)-subtable.StartCode[i]) - (n - i)
			return subtable.GlyphIdArray[index], true // index is always valid
		}
	}
	return 0, false
}

type cmapFormat6 struct {
	FirstCode    uint16
	GlyphIdArray []uint16
}

func (subtable *cmapFormat6) Get(r rune) (uint16, bool) {
	if r < int32(subtable.FirstCode) || uint32(len(subtable.GlyphIdArray)) <= uint32(r)-uint32(subtable.FirstCode) {
		return 0, false
	}
	return subtable.GlyphIdArray[uint32(r)-uint32(subtable.FirstCode)], true
}

type cmapFormat12 struct {
	StartCharCode []uint32
	EndCharCode   []uint32
	StartGlyphID  []uint32
}

func (subtable *cmapFormat12) Get(r rune) (uint16, bool) {
	if r < 0 {
		return 0, false
	}
	for i := 0; i < len(subtable.StartCharCode); i++ {
		if uint32(r) <= subtable.EndCharCode[i] && subtable.StartCharCode[i] <= uint32(r) {
			return uint16((uint32(r) - subtable.StartCharCode[i]) + subtable.StartGlyphID[i]), true
		}
	}
	return 0, false
}

type cmapEncodingRecord struct {
	PlatformID uint16
	EncodingID uint16
	Format     uint16
	Subtable   uint16
}

type cmapSubtable interface {
	Get(rune) (uint16, bool)
}

type cmapTable struct {
	EncodingRecords []cmapEncodingRecord
	Subtables       []cmapSubtable
}

func (t *cmapTable) Get(r rune) uint16 {
	for _, subtable := range t.Subtables {
		if glyphID, ok := subtable.Get(r); ok {
			return glyphID
		}
	}
	return 0
}

func (sfnt *SFNT) parseCmap() error {
	// requires data from maxp
	b, ok := sfnt.Tables["cmap"]
	if !ok {
		return fmt.Errorf("cmap: missing table")
	} else if len(b) < 4 {
		return fmt.Errorf("cmap: bad table")
	}

	sfnt.Cmap = &cmapTable{}
	r := newBinaryReader(b)
	if r.ReadUint16() != 0 {
		return fmt.Errorf("cmap: bad version")
	}
	numTables := r.ReadUint16()
	if uint32(len(b)) < 4+8*uint32(numTables) {
		return fmt.Errorf("cmap: bad table")
	}

	// find and extract subtables and make sure they don't overlap each other
	offsets, lengths := []uint32{0}, []uint32{4 + 8*uint32(numTables)}
	for j := 0; j < int(numTables); j++ {
		platformID := r.ReadUint16()
		encodingID := r.ReadUint16()
		subtableID := -1

		offset := r.ReadUint32()
		if uint32(len(b))-8 < offset { // subtable must be at least 8 bytes long to extract length
			return fmt.Errorf("cmap: bad subtable %d", j)
		}
		for i := 0; i < len(offsets); i++ {
			if offsets[i] < offset && offset < lengths[i] {
				return fmt.Errorf("cmap: bad subtable %d", j)
			}
		}

		// extract subtable length
		rs := newBinaryReader(b[offset:])
		format := rs.ReadUint16()
		var length uint32
		if format == 0 || format == 2 || format == 4 || format == 6 {
			length = uint32(rs.ReadUint16())
		} else if format == 8 || format == 10 || format == 12 || format == 13 {
			_ = rs.ReadUint16() // reserved
			length = rs.ReadUint32()
		} else if format == 14 {
			length = rs.ReadUint32()
		} else {
			return fmt.Errorf("cmap: bad format %d for subtable %d", format, j)
		}
		if length < 8 || math.MaxUint32-offset < length {
			return fmt.Errorf("cmap: bad subtable %d", j)
		}
		for i := 0; i < len(offsets); i++ {
			if offset == offsets[i] && length == lengths[i] {
				subtableID = int(i)
				break
			} else if offset <= offsets[i] && offsets[i] < offset+length {
				return fmt.Errorf("cmap: bad subtable %d", j)
			}
		}
		rs.buf = rs.buf[:length:length]

		if subtableID == -1 {
			subtableID = len(sfnt.Cmap.Subtables)
			offsets = append(offsets, offset)
			lengths = append(lengths, length)

			switch format {
			case 0:
				if rs.Len() != 258 {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}
				_ = rs.ReadUint16() // languageID

				subtable := &cmapFormat0{}
				copy(subtable.GlyphIdArray[:], rs.ReadBytes(256))
				for _, glyphID := range subtable.GlyphIdArray {
					if sfnt.Maxp.NumGlyphs <= uint16(glyphID) {
						return fmt.Errorf("cmap: bad glyphID in subtable %d", j)
					}
				}
				sfnt.Cmap.Subtables = append(sfnt.Cmap.Subtables, subtable)
			case 4:
				if rs.Len() < 10 {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}
				_ = rs.ReadUint16() // languageID

				segCount := rs.ReadUint16()
				if segCount%2 != 0 {
					return fmt.Errorf("cmap: bad segCount in subtable %d", j)
				}
				segCount /= 2
				if MaxCmapSegments < segCount {
					return fmt.Errorf("cmap: too many segments in subtable %d", j)
				}
				_ = rs.ReadUint16() // searchRange
				_ = rs.ReadUint16() // entrySelector
				_ = rs.ReadUint16() // rangeShift

				subtable := &cmapFormat4{}
				if rs.Len() < 2+8*uint32(segCount) {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}
				subtable.EndCode = make([]uint16, segCount)
				for i := 0; i < int(segCount); i++ {
					endCode := rs.ReadUint16()
					if 0 < i && endCode <= subtable.EndCode[i-1] {
						return fmt.Errorf("cmap: bad endCode in subtable %d", j)
					}
					subtable.EndCode[i] = endCode
				}
				_ = rs.ReadUint16() // reservedPad
				subtable.StartCode = make([]uint16, segCount)
				for i := 0; i < int(segCount); i++ {
					startCode := rs.ReadUint16()
					if subtable.EndCode[i] < startCode || 0 < i && startCode <= subtable.EndCode[i-1] {
						return fmt.Errorf("cmap: bad startCode in subtable %d", j)
					}
					subtable.StartCode[i] = startCode
				}
				subtable.IdDelta = make([]int16, segCount)
				for i := 0; i < int(segCount); i++ {
					subtable.IdDelta[i] = rs.ReadInt16()
				}

				glyphIdArrayLength := rs.Len() - 2*uint32(segCount)
				if glyphIdArrayLength%2 != 0 {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}
				glyphIdArrayLength /= 2

				subtable.IdRangeOffset = make([]uint16, segCount)
				for i := 0; i < int(segCount); i++ {
					idRangeOffset := rs.ReadUint16()
					if idRangeOffset%2 != 0 {
						return fmt.Errorf("cmap: bad idRangeOffset in subtable %d", j)
					} else if idRangeOffset != 0 {
						index := int(idRangeOffset/2) + int(subtable.EndCode[i]-subtable.StartCode[i]) - (int(segCount) - i)
						if index < 0 || glyphIdArrayLength <= uint32(index) {
							return fmt.Errorf("cmap: bad idRangeOffset in subtable %d", j)
						}
					}
					subtable.IdRangeOffset[i] = idRangeOffset
				}
				subtable.GlyphIdArray = make([]uint16, glyphIdArrayLength)
				for i := 0; i < int(glyphIdArrayLength); i++ {
					glyphID := rs.ReadUint16()
					if sfnt.Maxp.NumGlyphs <= glyphID {
						return fmt.Errorf("cmap: bad glyphID in subtable %d", j)
					}
					subtable.GlyphIdArray[i] = glyphID
				}
				sfnt.Cmap.Subtables = append(sfnt.Cmap.Subtables, subtable)
			case 6:
				if rs.Len() < 6 {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}
				_ = rs.ReadUint16() // language

				subtable := &cmapFormat6{}
				subtable.FirstCode = rs.ReadUint16()
				entryCount := rs.ReadUint16()
				if rs.Len() < 2*uint32(entryCount) {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}
				subtable.GlyphIdArray = make([]uint16, entryCount)
				for i := 0; i < int(entryCount); i++ {
					subtable.GlyphIdArray[i] = rs.ReadUint16()
				}
				sfnt.Cmap.Subtables = append(sfnt.Cmap.Subtables, subtable)
			case 12:
				if rs.Len() < 8 {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}
				_ = rs.ReadUint32() // language
				numGroups := rs.ReadUint32()
				if MaxCmapSegments < numGroups {
					return fmt.Errorf("cmap: too many segments in subtable %d", j)
				} else if rs.Len() < 12*numGroups {
					return fmt.Errorf("cmap: bad subtable %d", j)
				}

				subtable := &cmapFormat12{}
				subtable.StartCharCode = make([]uint32, numGroups)
				subtable.EndCharCode = make([]uint32, numGroups)
				subtable.StartGlyphID = make([]uint32, numGroups)
				for i := 0; i < int(numGroups); i++ {
					startCharCode := rs.ReadUint32()
					endCharCode := rs.ReadUint32()
					startGlyphID := rs.ReadUint32()
					if endCharCode < startCharCode || 0 < i && startCharCode <= subtable.EndCharCode[i-1] {
						return fmt.Errorf("cmap: bad character code range in subtable %d", j)
					} else if uint32(sfnt.Maxp.NumGlyphs) <= endCharCode-startCharCode || uint32(sfnt.Maxp.NumGlyphs)-(endCharCode-startCharCode) <= startGlyphID {
						return fmt.Errorf("cmap: bad glyphID in subtable %d", j)
					}
					subtable.StartCharCode[i] = startCharCode
					subtable.EndCharCode[i] = endCharCode
					subtable.StartGlyphID[i] = startGlyphID
				}
				sfnt.Cmap.Subtables = append(sfnt.Cmap.Subtables, subtable)
			}
		}
		sfnt.Cmap.EncodingRecords = append(sfnt.Cmap.EncodingRecords, cmapEncodingRecord{
			PlatformID: platformID,
			EncodingID: encodingID,
			Format:     format,
			Subtable:   uint16(subtableID),
		})
	}
	return nil
}

////////////////////////////////////////////////////////////////

type glyfContour struct {
	GlyphID                uint16
	XMin, YMin, XMax, YMax int16
	EndPoints              []uint16
	Instructions           []byte
	OnCurve                []bool
	XCoordinates           []int16
	YCoordinates           []int16
}

func (contour *glyfContour) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Glyph %v:\n", contour.GlyphID)
	fmt.Fprintf(&b, "  Contours: %v\n", len(contour.EndPoints))
	fmt.Fprintf(&b, "  XMin: %v\n", contour.XMin)
	fmt.Fprintf(&b, "  YMin: %v\n", contour.YMin)
	fmt.Fprintf(&b, "  XMax: %v\n", contour.XMax)
	fmt.Fprintf(&b, "  YMax: %v\n", contour.YMax)
	fmt.Fprintf(&b, "  EndPoints: %v\n", contour.EndPoints)
	fmt.Fprintf(&b, "  Instruction length: %v\n", len(contour.Instructions))
	fmt.Fprintf(&b, "  Coordinates:\n")
	for i := 0; i <= int(contour.EndPoints[len(contour.EndPoints)-1]); i++ {
		fmt.Fprintf(&b, "    ")
		if i < len(contour.XCoordinates) {
			fmt.Fprintf(&b, "%8v", contour.XCoordinates[i])
		} else {
			fmt.Fprintf(&b, "  ----  ")
		}
		if i < len(contour.YCoordinates) {
			fmt.Fprintf(&b, " %8v", contour.YCoordinates[i])
		} else {
			fmt.Fprintf(&b, "   ----  ")
		}
		if i < len(contour.OnCurve) {
			onCurve := "Off"
			if contour.OnCurve[i] {
				onCurve = "On"
			}
			fmt.Fprintf(&b, " %3v\n", onCurve)
		} else {
			fmt.Fprintf(&b, " ---\n")
		}
	}
	return b.String()
}

type glyfTable struct {
	data []byte
	loca *locaTable
}

func (glyf *glyfTable) Get(glyphID uint16) []byte {
	if len(glyf.loca.Offsets) <= int(glyphID)+1 {
		return nil
	}
	start := glyf.loca.Offsets[glyphID]
	end := glyf.loca.Offsets[glyphID+1]
	return glyf.data[start:end]
}

func (glyf *glyfTable) Contour(glyphID uint16, level int) (*glyfContour, error) {
	b := glyf.Get(glyphID)
	if b == nil {
		return nil, fmt.Errorf("glyf: bad glyphID %v", glyphID)
	} else if len(b) == 0 {
		return nil, nil
	}
	r := newBinaryReader(b)
	if r.Len() < 10 {
		return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
	}

	contour := &glyfContour{}
	contour.GlyphID = glyphID
	numberOfContours := r.ReadInt16()
	contour.XMin = r.ReadInt16()
	contour.YMin = r.ReadInt16()
	contour.XMax = r.ReadInt16()
	contour.YMax = r.ReadInt16()
	if 0 <= numberOfContours {
		// simple glyph
		if r.Len() < 2*uint32(numberOfContours)+2 {
			return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
		}
		contour.EndPoints = make([]uint16, numberOfContours)
		for i := 0; i < int(numberOfContours); i++ {
			contour.EndPoints[i] = r.ReadUint16()
		}

		instructionLength := r.ReadUint16()
		if r.Len() < uint32(instructionLength) {
			return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
		}
		contour.Instructions = r.ReadBytes(uint32(instructionLength))

		numPoints := int(contour.EndPoints[numberOfContours-1]) + 1
		flags := make([]byte, numPoints)
		contour.OnCurve = make([]bool, numPoints)
		for i := 0; i < numPoints; i++ {
			if r.Len() < 1 {
				return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
			}

			flags[i] = r.ReadByte()
			contour.OnCurve[i] = flags[i]&0x01 != 0
			if flags[i]&0x08 != 0 { // REPEAT_FLAG
				repeat := r.ReadByte()
				for j := 1; j <= int(repeat); j++ {
					flags[i+j] = flags[i]
					contour.OnCurve[i+j] = contour.OnCurve[i]
				}
				i += int(repeat)
			}
		}

		var x int16
		contour.XCoordinates = make([]int16, numPoints)
		for i := 0; i < numPoints; i++ {
			xShortVector := flags[i]&0x02 != 0
			xIsSameOrPositiveXShortVector := flags[i]&0x10 != 0
			if xShortVector {
				if r.Len() < 1 {
					return nil, fmt.Errorf("glyf: bad table or flags for glyphID %v", glyphID)
				}
				if xIsSameOrPositiveXShortVector {
					x += int16(r.ReadUint8())
				} else {
					x -= int16(r.ReadUint8())
				}
			} else if !xIsSameOrPositiveXShortVector {
				if r.Len() < 2 {
					return nil, fmt.Errorf("glyf: bad table or flags for glyphID %v", glyphID)
				}
				x += r.ReadInt16()
			}
			contour.XCoordinates[i] = x
		}

		var y int16
		contour.YCoordinates = make([]int16, numPoints)
		for i := 0; i < numPoints; i++ {
			yShortVector := flags[i]&0x04 != 0
			yIsSameOrPositiveYShortVector := flags[i]&0x20 != 0
			if yShortVector {
				if r.Len() < 1 {
					return nil, fmt.Errorf("glyf: bad table or flags for glyphID %v", glyphID)
				}
				if yIsSameOrPositiveYShortVector {
					y += int16(r.ReadUint8())
				} else {
					y -= int16(r.ReadUint8())
				}
			} else if !yIsSameOrPositiveYShortVector {
				if r.Len() < 2 {
					return nil, fmt.Errorf("glyf: bad table or flags for glyphID %v", glyphID)
				}
				y += r.ReadInt16()
			}
			contour.YCoordinates[i] = y
		}
	} else {
		if 7 < level {
			return nil, fmt.Errorf("glyf: compound glyphs too deeply nested")
		}

		// composite glyph
		for {
			if r.Len() < 4 {
				return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
			}

			flags := r.ReadUint16()
			subGlyphID := r.ReadUint16()
			if flags&0x0002 == 0 { // ARGS_ARE_XY_VALUES
				return nil, fmt.Errorf("glyf: composite glyph not supported")
			}
			var dx, dy int16
			if flags&0x0001 != 0 { // ARG_1_AND_2_ARE_WORDS
				if r.Len() < 4 {
					return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
				}
				dx = r.ReadInt16()
				dy = r.ReadInt16()
			} else {
				if r.Len() < 2 {
					return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
				}
				dx = int16(r.ReadInt8())
				dy = int16(r.ReadInt8())
			}
			var txx, txy, tyx, tyy int16
			if flags&0x0008 != 0 { // WE_HAVE_A_SCALE
				if r.Len() < 2 {
					return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
				}
				txx = r.ReadInt16()
				tyy = txx
			} else if flags&0x0040 != 0 { // WE_HAVE_AN_X_AND_Y_SCALE
				if r.Len() < 4 {
					return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
				}
				txx = r.ReadInt16()
				tyy = r.ReadInt16()
			} else if flags&0x0080 != 0 { // WE_HAVE_A_TWO_BY_TWO
				if r.Len() < 8 {
					return nil, fmt.Errorf("glyf: bad table for glyphID %v", glyphID)
				}
				txx = r.ReadInt16()
				txy = r.ReadInt16()
				tyx = r.ReadInt16()
				tyy = r.ReadInt16()
			}

			subContour, err := glyf.Contour(subGlyphID, level+1)
			if err != nil {
				return nil, err
			}

			var numPoints uint16
			if 0 < len(contour.EndPoints) {
				numPoints = contour.EndPoints[len(contour.EndPoints)-1] + 1
			}
			for i := 0; i < len(subContour.EndPoints); i++ {
				contour.EndPoints = append(contour.EndPoints, numPoints+subContour.EndPoints[i])
			}
			contour.OnCurve = append(contour.OnCurve, subContour.OnCurve...)
			for i := 0; i < len(subContour.XCoordinates); i++ {
				x := subContour.XCoordinates[i]
				y := subContour.YCoordinates[i]
				if flags&0x00C8 != 0 { // has transformation
					const half = 1 << 13
					xt := int16((int64(x)*int64(txx)+half)>>14) + int16((int64(y)*int64(tyx)+half)>>14)
					yt := int16((int64(x)*int64(txy)+half)>>14) + int16((int64(y)*int64(tyy)+half)>>14)
					x, y = xt, yt
				}
				contour.XCoordinates = append(contour.XCoordinates, dx+x)
				contour.YCoordinates = append(contour.YCoordinates, dy+y)
			}
			if flags&0x0020 == 0 { // MORE_COMPONENTS
				break
			}
		}
	}
	return contour, nil
}

func (sfnt *SFNT) parseGlyf() error {
	// requires data from loca
	b, ok := sfnt.Tables["glyf"]
	if !ok {
		return fmt.Errorf("glyf: missing table")
	} else if uint32(len(b)) != sfnt.Loca.Offsets[len(sfnt.Loca.Offsets)-1] {
		return fmt.Errorf("glyf: bad table")
	}

	sfnt.Glyf = &glyfTable{
		data: b,
		loca: sfnt.Loca,
	}
	return nil
}

////////////////////////////////////////////////////////////////

type headTable struct {
	FontRevision           uint32
	Flags                  [16]bool
	UnitsPerEm             uint16
	Created, Modified      time.Time
	XMin, YMin, XMax, YMax int16
	MacStyle               [16]bool
	LowestRecPPEM          uint16
	FontDirectionHint      int16
	IndexToLocFormat       int16
	GlyphDataFormat        int16
}

func (sfnt *SFNT) parseHead() error {
	b, ok := sfnt.Tables["head"]
	if !ok {
		return fmt.Errorf("head: missing table")
	} else if len(b) != 54 {
		return fmt.Errorf("head: bad table")
	}

	sfnt.Head = &headTable{}
	r := newBinaryReader(b)
	majorVersion := r.ReadUint16()
	minorVersion := r.ReadUint16()
	if majorVersion != 1 && minorVersion != 0 {
		return fmt.Errorf("head: bad version")
	}
	sfnt.Head.FontRevision = r.ReadUint32()
	_ = r.ReadUint32()                // checksumAdjustment
	if r.ReadUint32() != 0x5F0F3CF5 { // magicNumber
		return fmt.Errorf("head: bad magic version")
	}
	sfnt.Head.Flags = uint16ToFlags(r.ReadUint16())
	sfnt.Head.UnitsPerEm = r.ReadUint16()
	created := r.ReadUint64()
	modified := r.ReadUint64()
	if math.MaxInt64 < created || math.MaxInt64 < modified {
		return fmt.Errorf("head: created and/or modified dates too large")
	}
	sfnt.Head.Created = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Second * time.Duration(created))
	sfnt.Head.Modified = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Second * time.Duration(modified))
	sfnt.Head.XMin = r.ReadInt16()
	sfnt.Head.YMin = r.ReadInt16()
	sfnt.Head.XMax = r.ReadInt16()
	sfnt.Head.YMax = r.ReadInt16()
	sfnt.Head.MacStyle = uint16ToFlags(r.ReadUint16())
	sfnt.Head.LowestRecPPEM = r.ReadUint16()
	sfnt.Head.FontDirectionHint = r.ReadInt16()
	sfnt.Head.IndexToLocFormat = r.ReadInt16()
	if sfnt.Head.IndexToLocFormat != 0 && sfnt.Head.IndexToLocFormat != 1 {
		return fmt.Errorf("head: bad indexToLocFormat")
	}
	sfnt.Head.GlyphDataFormat = r.ReadInt16()
	return nil
}

////////////////////////////////////////////////////////////////

type hheaTable struct {
	Ascender            int16
	Descender           int16
	LineGap             int16
	AdvanceWidthMax     uint16
	MinLeftSideBearing  int16
	MinRightSideBearing int16
	XMaxExtent          int16
	CaretSlopeRise      int16
	CaretSlopeRun       int16
	CaretOffset         int16
	MetricDataFormat    int16
	NumberOfHMetrics    uint16
}

func (sfnt *SFNT) parseHhea() error {
	// requires data from maxp
	b, ok := sfnt.Tables["hhea"]
	if !ok {
		return fmt.Errorf("hhea: missing table")
	} else if len(b) != 36 {
		return fmt.Errorf("hhea: bad table")
	}

	sfnt.Hhea = &hheaTable{}
	r := newBinaryReader(b)
	majorVersion := r.ReadUint16()
	minorVersion := r.ReadUint16()
	if majorVersion != 1 && minorVersion != 0 {
		return fmt.Errorf("hhea: bad version")
	}
	sfnt.Hhea.Ascender = r.ReadInt16()
	sfnt.Hhea.Descender = r.ReadInt16()
	sfnt.Hhea.LineGap = r.ReadInt16()
	sfnt.Hhea.AdvanceWidthMax = r.ReadUint16()
	sfnt.Hhea.MinLeftSideBearing = r.ReadInt16()
	sfnt.Hhea.MinRightSideBearing = r.ReadInt16()
	sfnt.Hhea.XMaxExtent = r.ReadInt16()
	sfnt.Hhea.CaretSlopeRise = r.ReadInt16()
	sfnt.Hhea.CaretSlopeRun = r.ReadInt16()
	sfnt.Hhea.CaretOffset = r.ReadInt16()
	_ = r.ReadInt16() // reserved
	_ = r.ReadInt16() // reserved
	_ = r.ReadInt16() // reserved
	_ = r.ReadInt16() // reserved
	sfnt.Hhea.MetricDataFormat = r.ReadInt16()
	sfnt.Hhea.NumberOfHMetrics = r.ReadUint16()
	if sfnt.Maxp.NumGlyphs < sfnt.Hhea.NumberOfHMetrics || sfnt.Hhea.NumberOfHMetrics == 0 {
		return fmt.Errorf("hhea: bad numberOfHMetrics")
	}
	return nil
}

////////////////////////////////////////////////////////////////

type hmtxLongHorMetric struct {
	AdvanceWidth uint16
	Lsb          int16
}

type hmtxTable struct {
	HMetrics         []hmtxLongHorMetric
	LeftSideBearings []int16
}

func (hmtx *hmtxTable) LeftSideBearing(glyphID uint16) int16 {
	if uint16(len(hmtx.HMetrics)) <= glyphID {
		return hmtx.LeftSideBearings[glyphID-uint16(len(hmtx.HMetrics))]
	}
	return hmtx.HMetrics[glyphID].Lsb
}

func (hmtx *hmtxTable) Advance(glyphID uint16) uint16 {
	if uint16(len(hmtx.HMetrics)) <= glyphID {
		glyphID = uint16(len(hmtx.HMetrics)) - 1
	}
	return hmtx.HMetrics[glyphID].AdvanceWidth
}

func (sfnt *SFNT) parseHmtx() error {
	// requires data from hhea and maxp
	b, ok := sfnt.Tables["hmtx"]
	length := 4*uint32(sfnt.Hhea.NumberOfHMetrics) + 2*uint32(sfnt.Maxp.NumGlyphs-sfnt.Hhea.NumberOfHMetrics)
	if !ok {
		return fmt.Errorf("hmtx: missing table")
	} else if uint32(len(b)) != length {
		return fmt.Errorf("hmtx: bad table")
	}

	sfnt.Hmtx = &hmtxTable{}
	// numberOfHMetrics is smaller than numGlyphs
	sfnt.Hmtx.HMetrics = make([]hmtxLongHorMetric, sfnt.Hhea.NumberOfHMetrics)
	sfnt.Hmtx.LeftSideBearings = make([]int16, sfnt.Maxp.NumGlyphs-sfnt.Hhea.NumberOfHMetrics)

	r := newBinaryReader(b)
	for i := 0; i < int(sfnt.Hhea.NumberOfHMetrics); i++ {
		sfnt.Hmtx.HMetrics[i].AdvanceWidth = r.ReadUint16()
		sfnt.Hmtx.HMetrics[i].Lsb = r.ReadInt16()
	}
	for i := 0; i < int(sfnt.Maxp.NumGlyphs-sfnt.Hhea.NumberOfHMetrics); i++ {
		sfnt.Hmtx.LeftSideBearings[i] = r.ReadInt16()
	}
	return nil
}

////////////////////////////////////////////////////////////////

type kernPair struct {
	Key   uint32
	Value int16
}

type kernFormat0 struct {
	Coverage [8]bool
	Pairs    []kernPair
}

func (subtable *kernFormat0) Get(l, r uint16) int16 {
	key := uint32(l)<<16 | uint32(r)
	lo, hi := 0, len(subtable.Pairs)
	for lo < hi {
		mid := (lo + hi) / 2 // can be rounded down if odd
		pair := subtable.Pairs[mid]
		if pair.Key < key {
			lo = mid + 1
		} else if key < pair.Key {
			hi = mid
		} else {
			return pair.Value
		}
	}
	return 0
}

type kernTable struct {
	Subtables []kernFormat0
}

func (kern *kernTable) Get(l, r uint16) (k int16) {
	for _, subtable := range kern.Subtables {
		if !subtable.Coverage[1] {
			k += subtable.Get(l, r)
		} else if min := subtable.Get(l, r); k < min {
			// TODO: test
			k = min
		}
	}
	return
}

func (sfnt *SFNT) parseKern() error {
	b, ok := sfnt.Tables["kern"]
	if !ok {
		return fmt.Errorf("kern: missing table")
	} else if len(b) < 4 {
		return fmt.Errorf("kern: bad table")
	}

	r := newBinaryReader(b)
	version := r.ReadUint16()
	if version != 0 {
		// TODO: supported other kern versions
		return fmt.Errorf("kern: bad version")
	}

	nTables := r.ReadUint16()
	sfnt.Kern = &kernTable{}
	for j := 0; j < int(nTables); j++ {
		if r.Len() < 6 {
			return fmt.Errorf("kern: bad subtable %d", j)
		}

		subtable := kernFormat0{}
		startPos := r.Pos()
		subtableVersion := r.ReadUint16()
		if subtableVersion != 0 {
			// TODO: supported other kern subtable versions
			continue
		}
		length := r.ReadUint16()
		format := r.ReadUint8()
		subtable.Coverage = uint8ToFlags(r.ReadUint8())
		if format != 0 {
			// TODO: supported other kern subtable formats
			continue
		}
		if r.Len() < 8 {
			return fmt.Errorf("kern: bad subtable %d", j)
		}
		nPairs := r.ReadUint16()
		_ = r.ReadUint16() // searchRange
		_ = r.ReadUint16() // entrySelector
		_ = r.ReadUint16() // rangeShift
		if uint32(length) < 14+6*uint32(nPairs) {
			return fmt.Errorf("kern: bad length for subtable %d", j)
		}

		subtable.Pairs = make([]kernPair, nPairs)
		for i := 0; i < int(nPairs); i++ {
			subtable.Pairs[i].Key = r.ReadUint32()
			subtable.Pairs[i].Value = r.ReadInt16()
			if 0 < i && subtable.Pairs[i].Key <= subtable.Pairs[i-1].Key {
				return fmt.Errorf("kern: bad left right pair for subtable %d", j)
			}
		}

		// read unread bytes if length is bigger
		_ = r.ReadBytes(uint32(length) - (r.Pos() - startPos))
		sfnt.Kern.Subtables = append(sfnt.Kern.Subtables, subtable)
	}
	return nil
}

////////////////////////////////////////////////////////////////

type locaTable struct {
	Offsets []uint32
}

func (sfnt *SFNT) parseLoca() error {
	b, ok := sfnt.Tables["loca"]
	if !ok {
		return fmt.Errorf("loca: missing table")
	}

	sfnt.Loca = &locaTable{}
	sfnt.Loca.Offsets = make([]uint32, sfnt.Maxp.NumGlyphs+1)
	r := newBinaryReader(b)
	if sfnt.Head.IndexToLocFormat == 0 {
		if uint32(len(b)) != 2*(uint32(sfnt.Maxp.NumGlyphs)+1) {
			return fmt.Errorf("loca: bad table")
		}
		for i := 0; i < int(sfnt.Maxp.NumGlyphs+1); i++ {
			sfnt.Loca.Offsets[i] = uint32(r.ReadUint16())
			if 0 < i && sfnt.Loca.Offsets[i] < sfnt.Loca.Offsets[i-1] {
				return fmt.Errorf("loca: bad offsets")
			}
		}
	} else if sfnt.Head.IndexToLocFormat == 1 {
		if uint32(len(b)) != 4*(uint32(sfnt.Maxp.NumGlyphs)+1) {
			return fmt.Errorf("loca: bad table")
		}
		for i := 0; i < int(sfnt.Maxp.NumGlyphs+1); i++ {
			sfnt.Loca.Offsets[i] = r.ReadUint32()
			if 0 < i && sfnt.Loca.Offsets[i] < sfnt.Loca.Offsets[i-1] {
				return fmt.Errorf("loca: bad offsets")
			}
		}
	}
	return nil
}

////////////////////////////////////////////////////////////////

type maxpTable struct {
	NumGlyphs             uint16
	MaxPoints             uint16
	MaxContours           uint16
	MaxCompositePoints    uint16
	MaxCompositeContours  uint16
	MaxZones              uint16
	MaxTwilightPoints     uint16
	MaxStorage            uint16
	MaxFunctionDefs       uint16
	MaxInstructionDefs    uint16
	MaxStackElements      uint16
	MaxSizeOfInstructions uint16
	MaxComponentElements  uint16
	MaxComponentDepth     uint16
}

func (sfnt *SFNT) parseMaxp() error {
	b, ok := sfnt.Tables["maxp"]
	if !ok {
		return fmt.Errorf("maxp: missing table")
	}

	sfnt.Maxp = &maxpTable{}
	r := newBinaryReader(b)
	version := r.ReadBytes(4)
	sfnt.Maxp.NumGlyphs = r.ReadUint16()
	if binary.BigEndian.Uint32(version) == 0x00005000 && !sfnt.IsTrueType && len(b) == 6 {
		return nil
	} else if binary.BigEndian.Uint32(version) == 0x00010000 && !sfnt.IsCFF && len(b) == 32 {
		sfnt.Maxp.MaxPoints = r.ReadUint16()
		sfnt.Maxp.MaxContours = r.ReadUint16()
		sfnt.Maxp.MaxCompositePoints = r.ReadUint16()
		sfnt.Maxp.MaxCompositeContours = r.ReadUint16()
		sfnt.Maxp.MaxZones = r.ReadUint16()
		sfnt.Maxp.MaxTwilightPoints = r.ReadUint16()
		sfnt.Maxp.MaxStorage = r.ReadUint16()
		sfnt.Maxp.MaxFunctionDefs = r.ReadUint16()
		sfnt.Maxp.MaxInstructionDefs = r.ReadUint16()
		sfnt.Maxp.MaxStackElements = r.ReadUint16()
		sfnt.Maxp.MaxSizeOfInstructions = r.ReadUint16()
		sfnt.Maxp.MaxComponentElements = r.ReadUint16()
		sfnt.Maxp.MaxComponentDepth = r.ReadUint16()
		return nil
	}
	return fmt.Errorf("maxp: bad table")
}

////////////////////////////////////////////////////////////////

type nameNameRecord struct {
	PlatformID uint16
	EncodingID uint16
	LanguageID uint16
	NameID     uint16
	Offset     uint16
	Length     uint16
}

type nameLangTagRecord struct {
	Offset uint16
	Length uint16
}

type nameTable struct {
	NameRecord    []nameNameRecord
	LangTagRecord []nameLangTagRecord
	Data          []byte
}

func (sfnt *SFNT) parseName() error {
	b, ok := sfnt.Tables["name"]
	if !ok {
		return fmt.Errorf("name: missing table")
	} else if len(b) < 6 {
		return fmt.Errorf("name: bad table")
	}

	sfnt.Name = &nameTable{}
	r := newBinaryReader(b)
	version := r.ReadUint16()
	if version != 0 && version != 1 {
		return fmt.Errorf("name: bad version")
	}
	count := r.ReadUint16()
	_ = r.ReadUint16() // storageOffset
	if uint32(len(b)) < 6+12*uint32(count) {
		return fmt.Errorf("name: bad table")
	}
	sfnt.Name.NameRecord = make([]nameNameRecord, count)
	for i := 0; i < int(count); i++ {
		sfnt.Name.NameRecord[i].PlatformID = r.ReadUint16()
		sfnt.Name.NameRecord[i].EncodingID = r.ReadUint16()
		sfnt.Name.NameRecord[i].LanguageID = r.ReadUint16()
		sfnt.Name.NameRecord[i].NameID = r.ReadUint16()
		sfnt.Name.NameRecord[i].Length = r.ReadUint16()
		sfnt.Name.NameRecord[i].Offset = r.ReadUint16()
	}
	if version == 1 {
		if uint32(len(b)) < 6+12*uint32(count)+2 {
			return fmt.Errorf("name: bad table")
		}
		langTagCount := r.ReadUint16()
		if uint32(len(b)) < 6+12*uint32(count)+2+4*uint32(langTagCount) {
			return fmt.Errorf("name: bad table")
		}
		for i := 0; i < int(langTagCount); i++ {
			sfnt.Name.LangTagRecord[i].Length = r.ReadUint16()
			sfnt.Name.LangTagRecord[i].Offset = r.ReadUint16()
		}
	}
	sfnt.Name.Data = b[r.Pos():]
	return nil
}

////////////////////////////////////////////////////////////////

type os2Table struct {
	XAvgCharWidth           int16
	UsWeightClass           uint16
	UsWidthClass            uint16
	FsType                  uint16
	YSubscriptXSize         int16
	YSubscriptYSize         int16
	YSubscriptXOffset       int16
	YSubscriptYOffset       int16
	YSuperscriptXSize       int16
	YSuperscriptYSize       int16
	YSuperscriptXOffset     int16
	YSuperscriptYOffset     int16
	YStrikeoutSize          int16
	YStrikeoutPosition      int16
	SFamilyClass            int16
	BFamilyType             uint8
	BSerifStyle             uint8
	BWeight                 uint8
	BProportion             uint8
	BContrast               uint8
	BStrokeVariation        uint8
	BArmStyle               uint8
	BLetterform             uint8
	BMidline                uint8
	BXHeight                uint8
	UlUnicodeRange1         uint32
	UlUnicodeRange2         uint32
	UlUnicodeRange3         uint32
	UlUnicodeRange4         uint32
	AchVendID               [4]byte
	FsSelection             uint16
	UsFirstCharIndex        uint16
	UsLastCharIndex         uint16
	STypoAscender           int16
	STypoDescender          int16
	STypoLineGap            int16
	UsWinAscent             uint16
	UsWinDescent            uint16
	UlCodePageRange1        uint32
	UlCodePageRange2        uint32
	SxHeight                int16
	SCapHeight              int16
	UsDefaultChar           uint16
	UsBreakChar             uint16
	UsMaxContent            uint16
	UsLowerOpticalPointSize uint16
	UsUpperOpticalPointSize uint16
}

func (sfnt *SFNT) parseOS2() error {
	b, ok := sfnt.Tables["OS/2"]
	if !ok {
		return fmt.Errorf("OS/2: missing table")
	} else if len(b) < 68 {
		return fmt.Errorf("OS/2: bad table")
	}

	sfnt.OS2 = &os2Table{}
	r := newBinaryReader(b)
	version := r.ReadUint16()
	if 5 < version {
		return fmt.Errorf("OS/2: bad version")
	} else if version == 0 && len(b) != 68 && len(b) != 78 ||
		version == 1 && len(b) != 86 ||
		2 <= version && version <= 4 && len(b) != 96 ||
		version == 5 && len(b) != 100 {
		return fmt.Errorf("OS/2: bad table")
	}
	sfnt.OS2.XAvgCharWidth = r.ReadInt16()
	sfnt.OS2.UsWeightClass = r.ReadUint16()
	sfnt.OS2.UsWidthClass = r.ReadUint16()
	sfnt.OS2.FsType = r.ReadUint16()
	sfnt.OS2.YSubscriptXSize = r.ReadInt16()
	sfnt.OS2.YSubscriptYSize = r.ReadInt16()
	sfnt.OS2.YSubscriptXOffset = r.ReadInt16()
	sfnt.OS2.YSubscriptYOffset = r.ReadInt16()
	sfnt.OS2.YSuperscriptXSize = r.ReadInt16()
	sfnt.OS2.YSuperscriptYSize = r.ReadInt16()
	sfnt.OS2.YSuperscriptXOffset = r.ReadInt16()
	sfnt.OS2.YSuperscriptYOffset = r.ReadInt16()
	sfnt.OS2.YStrikeoutSize = r.ReadInt16()
	sfnt.OS2.YStrikeoutPosition = r.ReadInt16()
	sfnt.OS2.SFamilyClass = r.ReadInt16()
	sfnt.OS2.BFamilyType = r.ReadUint8()
	sfnt.OS2.BSerifStyle = r.ReadUint8()
	sfnt.OS2.BWeight = r.ReadUint8()
	sfnt.OS2.BProportion = r.ReadUint8()
	sfnt.OS2.BContrast = r.ReadUint8()
	sfnt.OS2.BStrokeVariation = r.ReadUint8()
	sfnt.OS2.BArmStyle = r.ReadUint8()
	sfnt.OS2.BLetterform = r.ReadUint8()
	sfnt.OS2.BMidline = r.ReadUint8()
	sfnt.OS2.BXHeight = r.ReadUint8()
	sfnt.OS2.UlUnicodeRange1 = r.ReadUint32()
	sfnt.OS2.UlUnicodeRange2 = r.ReadUint32()
	sfnt.OS2.UlUnicodeRange3 = r.ReadUint32()
	sfnt.OS2.UlUnicodeRange4 = r.ReadUint32()
	copy(sfnt.OS2.AchVendID[:], r.ReadBytes(4))
	sfnt.OS2.FsSelection = r.ReadUint16()
	sfnt.OS2.UsFirstCharIndex = r.ReadUint16()
	sfnt.OS2.UsLastCharIndex = r.ReadUint16()
	if 78 <= len(b) {
		sfnt.OS2.STypoAscender = r.ReadInt16()
		sfnt.OS2.STypoDescender = r.ReadInt16()
		sfnt.OS2.STypoLineGap = r.ReadInt16()
		sfnt.OS2.UsWinAscent = r.ReadUint16()
		sfnt.OS2.UsWinDescent = r.ReadUint16()
	}
	if version == 0 {
		return nil
	}
	sfnt.OS2.UlCodePageRange1 = r.ReadUint32()
	sfnt.OS2.UlCodePageRange2 = r.ReadUint32()
	if version == 1 {
		return nil
	}
	sfnt.OS2.SxHeight = r.ReadInt16()
	sfnt.OS2.SCapHeight = r.ReadInt16()
	sfnt.OS2.UsDefaultChar = r.ReadUint16()
	sfnt.OS2.UsBreakChar = r.ReadUint16()
	sfnt.OS2.UsMaxContent = r.ReadUint16()
	if 2 <= version && version <= 4 {
		return nil
	}
	sfnt.OS2.UsLowerOpticalPointSize = r.ReadUint16()
	sfnt.OS2.UsUpperOpticalPointSize = r.ReadUint16()
	return nil
}

////////////////////////////////////////////////////////////////

type postTable struct {
	ItalicAngle        uint32
	UnderlinePosition  int16
	UnderlineThickness int16
	IsFixedPitch       uint32
	MinMemType42       uint32
	MaxMemType42       uint32
	MinMemType1        uint32
	MaxMemType1        uint32
	GlyphName          []string
}

func (post *postTable) Get(glyphID uint16) string {
	if uint16(len(post.GlyphName)) <= glyphID {
		return ""
	}
	return post.GlyphName[glyphID]
}

func (sfnt *SFNT) parsePost() error {
	// requires data from maxp
	b, ok := sfnt.Tables["post"]
	if !ok {
		return fmt.Errorf("post: missing table")
	} else if len(b) < 32 {
		return fmt.Errorf("post: bad table")
	}

	sfnt.Post = &postTable{}
	r := newBinaryReader(b)
	version := r.ReadBytes(4)
	sfnt.Post.ItalicAngle = r.ReadUint32()
	sfnt.Post.UnderlinePosition = r.ReadInt16()
	sfnt.Post.UnderlineThickness = r.ReadInt16()
	sfnt.Post.IsFixedPitch = r.ReadUint32()
	sfnt.Post.MinMemType42 = r.ReadUint32()
	sfnt.Post.MaxMemType42 = r.ReadUint32()
	sfnt.Post.MinMemType1 = r.ReadUint32()
	sfnt.Post.MaxMemType1 = r.ReadUint32()
	if binary.BigEndian.Uint32(version) == 0x00010000 && !sfnt.IsCFF && len(b) == 32 {
		return nil
	} else if binary.BigEndian.Uint32(version) == 0x00020000 && !sfnt.IsCFF && 34 <= len(b) {
		if r.ReadUint16() != sfnt.Maxp.NumGlyphs {
			return fmt.Errorf("post: numGlyphs does not match maxp table numGlyphs")
		}
		if uint32(len(b)) < 34+2*uint32(sfnt.Maxp.NumGlyphs) {
			return fmt.Errorf("post: bad table")
		}

		// get string data first
		r.Seek(34 + 2*uint32(sfnt.Maxp.NumGlyphs))
		stringData := []string{}
		for 2 <= r.Len() {
			length := r.ReadUint8()
			if r.Len() < uint32(length) {
				return fmt.Errorf("post: bad table")
			}
			stringData = append(stringData, r.ReadString(uint32(length)))
		}
		if r.Len() != 0 {
			return fmt.Errorf("post: bad table")
		}

		r.Seek(34)
		sfnt.Post.GlyphName = make([]string, sfnt.Maxp.NumGlyphs)
		for i := 0; i < int(sfnt.Maxp.NumGlyphs); i++ {
			index := r.ReadUint16()
			if index < 258 {
				sfnt.Post.GlyphName[i] = macintoshGlyphNames[index]
			} else if len(stringData) < int(index)-258 {
				return fmt.Errorf("post: bad table")
			} else {
				sfnt.Post.GlyphName[i] = stringData[index-258]
			}
		}
		return nil
	} else if binary.BigEndian.Uint32(version) == 0x00025000 && len(b) == 32 {
		return fmt.Errorf("post: version 2.5 not supported")
	} else if binary.BigEndian.Uint32(version) == 0x00030000 && len(b) == 32 {
		return nil
	}
	return fmt.Errorf("post: bad table")
}
