// Package ssm implements the Parameter Store part of AWS Systems Manager, as
// LocalStack serves it through moto: parameters with their versions, labels,
// hierarchy and tags, and the maintenance-window records moto keeps. Every
// other SSM operation answers 501 through the generated Unimplemented.
//
// SecureString values are stored as moto stores them, "kms:<key id>:<value>",
// and nothing calls KMS.
package ssm
