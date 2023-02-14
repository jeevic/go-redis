module github.com/jeevic/go-redis/extra/redisotel/v9

go 1.15

replace github.com/jeevic/go-redis/v9 => ../..

replace github.com/jeevic/go-redis/extra/rediscmd/v9 => ../rediscmd

require (
	github.com/jeevic/go-redis/extra/rediscmd/v9 v9.0.2
	github.com/jeevic/go-redis/v9 v9.0.2
	go.opentelemetry.io/otel v1.12.0
	go.opentelemetry.io/otel/metric v0.35.0
	go.opentelemetry.io/otel/sdk v1.12.0
	go.opentelemetry.io/otel/trace v1.12.0
)
