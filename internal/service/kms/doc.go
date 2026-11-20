// Package kms is KMS, after LocalStack's own provider
// (localstack/services/kms: provider.py, models.py, utils.py), which uses no
// moto.
//
// A key lives in the State of its account and region. Any of its ids reaches
// it: the key id, its ARN, an alias name and an alias ARN, through one
// resolver that answers LocalStack's errors. LocalStack's reserved aliases
// (alias/aws/s3 and the like) make their key on first use, so a read can change
// state; it says so through the provider's changed function.
//
// A data operation finds its key in two steps, in LocalStack's order: dataKey
// resolves the id, taking an account and region from a key ARN, and refuses a
// disabled key or one pending deletion; the operation's own checks of its input
// come next; then Key.usable refuses a key of another usage or one still
// waiting for its material.
//
// The symmetric ciphertext is LocalStack's, in the ciphertext package.
//
// A grant lives in the State of the caller's account and region, not the key's,
// as in LocalStack, and nothing enforces it. GenerateMac and VerifyMac take
// their key through the same two steps as a data operation, with
// GENERATE_VERIFY_MAC as the usage.
//
// KMS reads no settings: LocalStack's provider reads nothing from its config
// beyond KMS_PROVIDER, which picks among LocalStack's own implementations.
package kms
