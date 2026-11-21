// Ported from UxPlay's lib/playfair (GPL-3.0-or-later).
// Copyright (C) 2012 Juho Vahae-Herttua and contributors.
// See LICENSE at the repository root.

package fairplay

import (
	"encoding/binary"
	"math"
	"math/bits"
)

// All word loads/stores in PlayFair are little endian. Explicit conversion
// keeps the original cipher independent of host endianness and alignment.
var little = binary.LittleEndian

var shiftRows = [16]int{0, 13, 10, 7, 4, 1, 14, 11, 8, 5, 2, 15, 12, 9, 6, 3}
var firstTables = [16]int{0, 5, 10, 15, 4, 9, 14, 3, 8, 13, 2, 7, 12, 1, 6, 11}

func playfairDecrypt(message, encrypted []byte) []byte {
	key := generateSessionKey(message)
	schedule := generateKeySchedule(key[:])
	var block [16]byte
	for i := range block {
		block[i] = encrypted[56+i] ^ zKey[i]
	}
	cycle(block[:], &schedule)
	for i := range block {
		block[i] ^= encrypted[16+i] ^ xKey[i] ^ zKey[i]
	}
	return block[:]
}

func generateKeySchedule(key []byte) (schedule [11][4]uint32) {
	var buffer [16]byte
	for i := range buffer {
		buffer[i] = key[i] ^ tKey[i]
	}
	for round := range schedule {
		schedule[round][0] = little.Uint32(buffer[:])
		for i := range 4 {
			table := ((31 * (round*4 + i)) % 40) << 8
			buffer[i] ^= tableS1[table+int(buffer[12+(i+1)%4])]
		}
		buffer[0] ^= indexMangle[round]
		for word := 1; word < 4; word++ {
			off := word * 4
			schedule[round][word] = little.Uint32(buffer[off:])
			little.PutUint32(buffer[off:], schedule[round][word]^little.Uint32(buffer[off-4:]))
		}
	}
	return
}

func cycle(block []byte, schedule *[11][4]uint32) {
	for i := range 4 {
		little.PutUint32(block[i*4:], little.Uint32(block[i*4:])^schedule[10][i])
	}
	var previous [16]byte
	copy(previous[:], block)
	for dest, source := range shiftRows {
		block[dest] = tableS3[firstTables[dest]*256+int(previous[source])]
	}
	for round := range 9 {
		for word := range 4 {
			off := word * 4
			key := schedule[9-round][word]
			value := tableS8[block[off]^byte(key)] ^
				tableS7[block[off+1]^byte(key>>8)] ^
				tableS6[block[off+2]^byte(key>>16)] ^
				tableS5[block[off+3]^byte(key>>24)]
			little.PutUint32(block[off:], value)
		}
		copy(previous[:], block)
		for dest, source := range shiftRows {
			table := ((71 * ((8-round)*16 + dest)) % 144) << 8
			block[dest] = tableS4[table+int(previous[source])]
		}
	}
	for i := range 4 {
		little.PutUint32(block[i*4:], little.Uint32(block[i*4:])^schedule[0][i])
	}
}

func decryptMessage(message []byte) (out [128]byte) {
	mode := int(message[12])
	for chunk := range 8 {
		off := (chunk + 1) * 16
		if mode == 3 {
			off = 128 - chunk*16
		}
		var block, previous [16]byte
		copy(block[:], message[off:off+16])
		for round := range 9 {
			base := 128 - round*16
			previous = block
			for dest, source := range shiftRows {
				table := ((97 * (base + dest)) % 144) << 8
				block[dest] = tableS2[table+int(previous[source])] ^ messageKey[mode][base+dest]
			}
			for word := range 4 {
				i := word * 4
				value := tableS9[int(block[i])] ^ tableS9[256+int(block[i+1])] ^
					tableS9[512+int(block[i+2])] ^ tableS9[768+int(block[i+3])]
				little.PutUint32(block[i:], value)
			}
		}
		previous = block
		for dest, source := range shiftRows {
			block[dest] = tableS10[dest*256+int(previous[source])]
		}
		outOff := chunk * 16
		iv := messageIv[mode][:]
		if mode == 3 {
			outOff = 112 - chunk*16
			if chunk < 7 {
				iv = message[outOff:]
			}
		} else if chunk > 0 {
			iv = message[outOff:]
		}
		for i := range block {
			out[outOff+i] = block[i] ^ iv[i]
		}
	}
	return
}

func generateSessionKey(message []byte) [16]byte {
	decrypted := decryptMessage(message)
	var sap [320]byte
	copy(sap[:17], staticSource1[:])
	copy(sap[17:145], decrypted[:])
	copy(sap[145:273], defaultSap[128:256])
	copy(sap[273:], staticSource2[:])
	key := initialSessionKey
	for round := range 5 {
		base := sap[round*64 : (round+1)*64]
		md5 := modifiedMD5(base, key[:])
		key = sapHash(base)
		for i := range 4 {
			off := i * 4
			little.PutUint32(key[off:], little.Uint32(key[off:])+little.Uint32(md5[off:]))
		}
	}
	for off := 0; off < 16; off += 4 {
		key[off], key[off+3] = key[off+3], key[off]
		key[off+1], key[off+2] = key[off+2], key[off+1]
	}
	for i := range key {
		key[i] ^= 121
	}
	return key
}

var md5Shifts = [64]int{
	7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
	5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
	4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
	6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
}

var md5Constants = func() (constants [64]uint32) {
	for i := range constants {
		constants[i] = uint32(uint64(math.Abs(math.Sin(float64(i+1))) * (1 << 32)))
	}
	return
}()

func modifiedMD5(input, key []byte) (out [16]byte) {
	var block [64]byte
	copy(block[:], input)
	a, b, c, d := little.Uint32(key), little.Uint32(key[4:]), little.Uint32(key[8:]), little.Uint32(key[12:])
	for i := range 64 {
		j, f := i, (b&c)|(^b&d)
		switch {
		case i >= 48:
			j, f = 7*i%16, c^(b|^d)
		case i >= 32:
			j, f = (3*i+5)%16, b^c^d
		case i >= 16:
			j, f = (5*i+1)%16, (b&d)|(c&^d)
		}
		z := a + binary.BigEndian.Uint32(block[j*4:]) + md5Constants[i] + f
		a, b, c, d = d, b+bits.RotateLeft32(z, md5Shifts[i]), b, c
		if i == 31 {
			for _, pair := range [][2]uint32{{a & 15, b & 15}, {c & 15, d & 15}, {(a >> 4) & 15, (b >> 4) & 15}, {(a >> 8) & 15, (b >> 8) & 15}, {(a >> 12) & 15, (b >> 12) & 15}} {
				x, y := pair[0]*4, pair[1]*4
				left, right := little.Uint32(block[x:]), little.Uint32(block[y:])
				little.PutUint32(block[x:], right)
				little.PutUint32(block[y:], left)
			}
		}
	}
	for i, word := range [...]uint32{a, b, c, d} {
		little.PutUint32(out[i*4:], little.Uint32(key[i*4:])+word)
	}
	return
}

func sapHash(input []byte) (out [16]byte) {
	buffer0 := []byte{0x96, 0x5f, 0xc6, 0x53, 0xf8, 0x46, 0xcc, 0x18, 0xdf, 0xbe, 0xb2, 0xf8, 0x38, 0xd7, 0xec, 0x22, 0x03, 0xd1, 0x20, 0x8f}
	var buffer1 [210]byte
	buffer2 := []byte{0x43, 0x54, 0x62, 0x7a, 0x18, 0xc3, 0xd6, 0xb3, 0x9a, 0x56, 0xf6, 0x1c, 0x14, 0x3f, 0x0c, 0x1d, 0x3b, 0x36, 0x83, 0xb1, 0x39, 0x51, 0x4a, 0xaa, 0x09, 0x3e, 0xfe, 0x44, 0xaf, 0xde, 0xc3, 0x20, 0x9d, 0x42, 0x3a}
	var buffer3 [132]byte
	buffer4 := []byte{0xed, 0x25, 0xd1, 0xbb, 0xbc, 0x27, 0x9f, 0x02, 0xa2, 0xa9, 0x11, 0x00, 0x0c, 0xb3, 0x52, 0xc0, 0xbd, 0xe3, 0x1b, 0x49, 0xc7}
	for i := range buffer1 {
		buffer1[i] = input[(i%64)&^3+3-i%4]
	}
	for i := range 840 {
		x := buffer1[uint32(i-155)%210]
		y := buffer1[uint32(i-57)%210]
		z := buffer1[uint32(i-13)%210]
		w := buffer1[i%210]
		buffer1[i%210] = bits.RotateLeft8(y, 5) + (bits.RotateLeft8(z, 3) ^ w) - bits.RotateLeft8(x, 7)
	}
	garble(buffer0, buffer1[:], buffer2, buffer3[:], buffer4)
	for i := range out {
		out[i] = 0xe1
	}
	for i, index := range [...]int{18, 22, 23, 0, 5, 19, 32, 31, 10, 21, 30} {
		if i == 3 {
			out[i] = 0x3d
		} else {
			out[i] += buffer3[index*4]
		}
	}
	for _, buffer := range [][]byte{buffer0, buffer2, buffer1[:]} {
		for i, value := range buffer {
			out[i%16] ^= value
		}
	}
	for range 16 {
		for i := range out {
			out[i] = bits.RotateLeft8(out[(i+9)%16], 1) ^ out[i] ^ bits.RotateLeft8(out[(i+11)%16], 6) ^ bits.RotateLeft8(out[(i+15)%16], 5)
		}
	}
	return
}

func rotateByte(input, count uint32) uint32 { return uint32(bits.RotateLeft8(byte(input), int(count))) }
func rotateByteWide(input, count uint32) uint32 {
	input &= 255
	return input<<count | input>>(8-count)
}
func weirdRotateRight(input, count uint32) uint32 {
	if count == 0 {
		return 0
	}
	input &= 255
	return input>>count | input<<(8-count)
}
func weirdRotateLeft(input, count uint32) uint32 {
	if count == 0 {
		return 0
	}
	return rotateByte(input, count)
}
func weirdRotate32(input, count uint32) uint32 {
	if count == 0 {
		return 0
	}
	input &= 255
	return input<<count ^ input>>(8-count)
}
