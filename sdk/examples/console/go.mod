module github.com/stellar/experimental-paymentment-channels/examples/console

go 1.16

replace github.com/stellar/go => github.com/leighmcculloch/stellar--go v0.0.0-20210528222607-c2e3ef441a5d

replace github.com/stellar/experimental-payment-channels/sdk => ../..

require (
	github.com/stellar/experimental-payment-channels/sdk v0.0.0-00010101000000-000000000000
	github.com/stellar/go v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.6.1 // indirect
	golang.org/x/sys v0.0.0-20201119102817-f84b799fce68 // indirect
	gopkg.in/yaml.v2 v2.3.0 // indirect
)
