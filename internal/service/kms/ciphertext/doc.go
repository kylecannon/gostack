// Package ciphertext is the symmetric ciphertext of LocalStack's KMS, which
// LocalStack's own tests build and take apart: the key id in 36 bytes, a 16-byte
// IV, the 16-byte GCM tag, then the AES-GCM ciphertext of the PKCS7-padded
// plaintext. The additional data is the encryption context, its pairs sorted by
// key and concatenated (models.py: _serialize_ciphertext_blob,
// _serialize_encryption_context; utils/crypto.py: encrypt, decrypt).
package ciphertext
