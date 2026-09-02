// Package steamcrypto implements the symmetric scheme Steam uses for depot
// chunks, manifest filenames and encrypted branch manifest ids.
package steamcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
)

// SymmetricDecrypt handles Steam's "ECB-encrypted IV then CBC body" layout
// with PKCS#7 padding.
func SymmetricDecrypt(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data) < 32 || len(data)%16 != 0 {
		return nil, errors.New("steamcrypto: ciphertext length invalid")
	}
	iv := make([]byte, 16)
	block.Decrypt(iv, data[:16])

	body := make([]byte, len(data)-16)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(body, data[16:])
	return unpad(body)
}

// DecryptECB decrypts whole blocks with no IV, used for encrypted branch
// manifest ids.
func DecryptECB(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%16 != 0 {
		return nil, errors.New("steamcrypto: ECB input not block aligned")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += 16 {
		block.Decrypt(out[i:i+16], data[i:i+16])
	}
	return unpad(out)
}

func unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("steamcrypto: empty plaintext")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > 16 || n > len(b) {
		return nil, errors.New("steamcrypto: bad padding")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("steamcrypto: bad padding")
		}
	}
	return b[:len(b)-n], nil
}

// Adler is Steam's checksum for chunk contents. It is Adler-32 with both
// accumulators starting at zero instead of the standard one.
func Adler(data []byte) uint32 {
	var a, b uint32
	for _, c := range data {
		a = (a + uint32(c)) % 65521
		b = (b + a) % 65521
	}
	return a | b<<16
}

// SymmetricEncrypt is the inverse of SymmetricDecrypt. Steam never needs
// it from a client; it exists so tests can build realistic chunks.
func SymmetricEncrypt(plain, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != 16 {
		return nil, errors.New("steamcrypto: iv must be 16 bytes")
	}
	n := 16 - len(plain)%16
	padded := make([]byte, 0, len(plain)+n)
	padded = append(padded, plain...)
	for i := 0; i < n; i++ {
		padded = append(padded, byte(n))
	}
	out := make([]byte, 16+len(padded))
	block.Encrypt(out[:16], iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[16:], padded)
	return out, nil
}
