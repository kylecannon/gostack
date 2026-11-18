// Package config resolves gostack's settings from the environment.
//
// Every variable is looked up through viper, in the order GOSTACK_<NAME>,
// LOCALSTACK_<NAME>, then <NAME>. Variables that exist only in gostack have only
// the GOSTACK_ form, and variables whose LocalStack name already starts with
// LOCALSTACK_ (LOCALSTACK_HOST) have GOSTACK_<NAME> and <NAME>.
//
// Load also exports each LOCALSTACK_<NAME> variable as the bare <NAME> into the
// process environment, with the exclusions of LocalStack's
// bin/docker-entrypoint.sh, so hook subprocesses inherit it.
//
// A [Config] hands out immutable [Settings] snapshots. [Config.Set] applies a
// runtime update, which is how POST /_localstack/config works, and swaps in a
// new snapshot.
package config
