module github.com/wamra/session-index

go 1.24.1

require (
	github.com/ncruces/go-sqlite3 v0.21.3
	github.com/tetratelabs/wazero v1.9.0
)

require (
	github.com/aws/aws-sdk-go-v2 v1.48.0
	github.com/aws/aws-sdk-go-v2/config v1.33.18
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.63.13
	github.com/aws/smithy-go v1.28.9
	github.com/klauspost/compress v1.19.2
	golang.org/x/crypto v0.31.0
	golang.org/x/term v0.32.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.27 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.21.0 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.25 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.13.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.52.0 // indirect
	lukechampine.com/adiantum v1.1.1 // indirect
)

require (
	github.com/asg017/sqlite-vec-go-bindings v0.1.6
	github.com/ncruces/julianday v1.0.0 // indirect
	golang.org/x/sys v0.33.0
)

replace github.com/ncruces/go-sqlite3 => github.com/wamra/go-sqlite3 v0.21.4-0.20261122010613-18695619c5b1
