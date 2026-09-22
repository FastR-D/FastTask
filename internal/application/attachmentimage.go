package application

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// Image handling for attachments (doc/chat-features.md §4.5).
//
// Three rules, all server-side because none of them can be enforced anywhere else:
//
//  1. The type is sniffed from the magic number. A Content-Type header and a file extension are claims a
//     client makes, and an upload endpoint that trusts them becomes a way to store arbitrary bytes under
//     an image's name.
//  2. EXIF is stripped before the bytes touch the disk. Phone photos of lab notebooks carry GPS, and a
//     research deployment is exactly where that must not end up in a database.
//  3. Dimensions come from the stripped bytes, so what the API reports is what is stored.

// allowedImageMimes is the whitelist (§4.5). Anything else is a 400 with a message that names the type.
var allowedImageMimes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// ErrUnsupportedImage is returned for bytes that are not one of the whitelisted types.
var ErrUnsupportedImage = errors.New("unsupported image type")

// sniffImageMime determines the real type from the first bytes. It does not use http.DetectContentType
// alone: that reports image/jpeg for some EXIF-heavy files and "application/octet-stream" for webp, so the
// signatures are checked explicitly and in the order that cannot be confused.
func sniffImageMime(data []byte) (string, bool) {
	switch {
	case len(data) >= 8 && bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png", true
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", true
	case len(data) >= 6 && bytes.HasPrefix(data, []byte("GIF87a")), len(data) >= 6 && bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif", true
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "image/webp", true
	}
	return "", false
}

// imageExtension is the suffix a stored file gets, from the sniffed type.
func imageExtension(mime string) string {
	return allowedImageMimes[mime]
}

// imageDimensions reads width and height without decoding pixels. WebP has no stdlib config decoder, so
// its container is parsed directly; every other type goes through image.DecodeConfig.
func imageDimensions(mime string, data []byte) (int, int) {
	if mime == "image/webp" {
		return webpDimensions(data)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return config.Width, config.Height
}

// webpDimensions reads the dimensions out of a RIFF/WEBP container (VP8X, VP8 or VP8L).
func webpDimensions(data []byte) (int, int) {
	if len(data) < 30 {
		return 0, 0
	}
	switch string(data[12:16]) {
	case "VP8X":
		// The extended header stores width-1 and height-1 in 24-bit little endian fields.
		width := int(uint32(data[24])|uint32(data[25])<<8|uint32(data[26])<<16) + 1
		height := int(uint32(data[27])|uint32(data[28])<<8|uint32(data[29])<<16) + 1
		return width, height
	case "VP8 ":
		// Lossy: the frame header starts at the chunk payload, 10 bytes in past the chunk header and the
		// frame tag, and carries a 0x9d012a sync code.
		offset := 20
		if len(data) < offset+10 {
			return 0, 0
		}
		for i := offset; i+4 < len(data); i++ {
			if data[i] == 0x9d && data[i+1] == 0x01 && data[i+2] == 0x2a {
				width := int(binary.LittleEndian.Uint16(data[i+3:i+5])) & 0x3fff
				height := int(binary.LittleEndian.Uint16(data[i+5:i+7])) & 0x3fff
				return width, height
			}
		}
	case "VP8L":
		// Lossless: 14 bits each of width-1 and height-1 after the 0x2f signature.
		offset := 21
		if len(data) < offset+4 {
			return 0, 0
		}
		bits := binary.LittleEndian.Uint32(data[offset : offset+4])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1
	}
	return 0, 0
}

// stripImageMetadata removes EXIF (and with it GPS) from the whitelisted formats. It is deliberately
// conservative: a container it does not fully understand is rejected rather than passed through, because
// the failure mode of "pass it through" is a location leaking into a research database.
func stripImageMetadata(mime string, data []byte) ([]byte, error) {
	switch mime {
	case "image/jpeg":
		return stripJPEGMarkers(data)
	case "image/png":
		return stripPNGChunks(data)
	case "image/webp":
		return stripWebPChunks(data)
	case "image/gif":
		// GIF has no EXIF segment; the format predates it and carries only the logical screen descriptor
		// and a comment extension, neither of which holds geolocation.
		return data, nil
	}
	return nil, ErrUnsupportedImage
}

// stripJPEGMarkers rebuilds a JPEG without its metadata segments. APP1 (EXIF/XMP), APP3 (Meta) and COM are
// dropped; APP0 (JFIF) and the DQT/DHT/SOS structure are kept, because those are what makes the image
// decodable.
func stripJPEGMarkers(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, ErrUnsupportedImage
	}
	out := make([]byte, 0, len(data))
	out = append(out, 0xff, 0xd8)
	i := 2
	for i < len(data) {
		if data[i] != 0xff {
			return nil, ErrUnsupportedImage
		}
		// Padding fill bytes are legal before a marker; markerStart keeps the 0xff that belongs to it,
		// so a copied segment is still a segment.
		for i < len(data) && data[i] == 0xff {
			i++
		}
		if i >= len(data) {
			return nil, ErrUnsupportedImage
		}
		markerStart := i - 1
		marker := data[i]
		i++
		// Markers with no payload.
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			out = append(out, data[markerStart:i]...)
			continue
		}
		if i+2 > len(data) {
			return nil, ErrUnsupportedImage
		}
		size := int(binary.BigEndian.Uint16(data[i : i+2]))
		if size < 2 || i+size > len(data) {
			return nil, ErrUnsupportedImage
		}
		segment := data[markerStart : i+size]
		switch {
		case marker == 0xda:
			// Start of scan: everything after the header is entropy-coded data with no further metadata,
			// so the remainder is copied verbatim.
			out = append(out, segment...)
			out = append(out, data[i+size:]...)
			return out, nil
		case marker == 0xe1, marker == 0xe3, marker == 0xfe:
			// APP1 (EXIF, XMP), APP3 (Meta), COM (comment): dropped.
		default:
			out = append(out, segment...)
		}
		i += size
	}
	return nil, ErrUnsupportedImage
}

// stripPNGChunks rebuilds a PNG without ancillary metadata. eXIf, tEXt, iTXt, zTXt and tIME are dropped;
// everything critical (IHDR, PLTE, IDAT, IEND) and colour-related (gAMA, cHRM, sRGB, iCCP) is kept, so the
// image renders identically.
func stripPNGChunks(data []byte) ([]byte, error) {
	signature := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if len(data) < 8 || !bytes.HasPrefix(data, signature) {
		return nil, ErrUnsupportedImage
	}
	out := append([]byte(nil), signature...)
	dropped := map[string]bool{"eXIf": true, "tEXt": true, "iTXt": true, "zTXt": true, "tIME": true}
	for i := 8; i+8 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[i : i+4]))
		kind := string(data[i+4 : i+8])
		end := i + 12 + length
		if length < 0 || end > len(data) {
			return nil, ErrUnsupportedImage
		}
		if !dropped[kind] {
			out = append(out, data[i:end]...)
		}
		if kind == "IEND" {
			return out, nil
		}
		i = end
	}
	return nil, ErrUnsupportedImage
}

// stripWebPChunks rebuilds a RIFF/WEBP container without its EXIF and XMP chunks, promoting a simple
// VP8/VP8L file unchanged.
func stripWebPChunks(data []byte) ([]byte, error) {
	if len(data) < 12 || !bytes.HasPrefix(data, []byte("RIFF")) || string(data[8:12]) != "WEBP" {
		return nil, ErrUnsupportedImage
	}
	out := make([]byte, 0, len(data))
	out = append(out, []byte("RIFF")...)
	out = append(out, 0, 0, 0, 0) // size, patched at the end
	out = append(out, []byte("WEBP")...)
	for i := 12; i+8 <= len(data); {
		kind := string(data[i : i+4])
		length := int(binary.LittleEndian.Uint32(data[i+4 : i+8]))
		end := i + 8 + length
		if length < 0 || end > len(data) {
			return nil, ErrUnsupportedImage
		}
		if kind != "EXIF" && kind != "XMP " {
			out = append(out, data[i:end]...)
			if length%2 == 1 && end+1 <= len(data) {
				out = append(out, data[end]) // RIFF chunks are padded to an even size
				end++
			}
		} else if length%2 == 1 && end+1 <= len(data) {
			end++
		}
		i = end
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out, nil
}
