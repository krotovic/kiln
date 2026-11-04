//! Kiln's Rust runtime. Kiln is a statically typed subset of TypeScript for embedding: its Go compiler
//! (`github.com/krotovic/kiln/language`) turns scripts into WebAssembly modules, which this crate runs on wasmtime
//! the way the Go host (wazero) and the browser's (`@krotovic/kiln`) do, through the same interface
//! (`language/abi.go`); and it evaluates Kiln's expressions with its own interpreter, as Go does.
//!
//! - [`load_script`] loads a compiled module against the [`HostModule`]s it was compiled with; [`Script`] makes
//!   [`Instance`]s of its classes and calls their methods with [`Value`]s.
//! - [`expr`] compiles and evaluates expressions.

pub mod expr;
pub mod gofmt;
mod script;
mod value;

pub use script::{
    Call, Class, Error, HostModule, Instance, Interface, InterfaceMember, MAX_PENDING_PROMISES, NativeError,
    NativeResult, Script, ScriptError, load_script,
};
pub use value::{Kind, MapValue, NativeObject, Resolver, Value};
