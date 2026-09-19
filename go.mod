module github.com/colonelpanic8/google-messages-multidevice-bridge

go 1.26.7

require (
	github.com/SherClockHolmes/webpush-go v1.4.0
	github.com/rs/zerolog v1.35.1
	go.etcd.io/bbolt v1.4.3
	go.mau.fi/mautrix-gmessages v0.2608.1-0.20260911171723-e6cc29974f92
	go.mau.fi/util v0.10.1
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/golang-jwt/jwt/v5 v5.2.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace go.mau.fi/mautrix-gmessages => ./third_party/mautrix-gmessages
