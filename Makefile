.PHONY: all rust go test rust-test go-test snippets clean

all: rust go

rust:
	cd rust && cargo build --release --locked
	cp rust/target/release/indexnudge bin-rust/indexnudge

go:
	cd go && go build -o ../bin-go/indexnudge .

rust-test:
	cd rust && cargo test --locked

go-test:
	cd go && go test ./...

test: rust-test go-test
	bash -n bin/sqlite3

snippets:
	gpp AGENTS.md.gpp > AGENTS.md.snippet
	gpp -DLANG_RUST AGENTS.md.gpp > AGENTS.md.rust.snippet
	gpp -DLANG_GO AGENTS.md.gpp > AGENTS.md.go.snippet

clean:
	cd rust && cargo clean
	rm -f bin-rust/indexnudge bin-go/indexnudge
