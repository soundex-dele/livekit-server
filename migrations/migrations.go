package migrations

import _ "embed"

//go:embed remote-control/001_sessions.sql
var RemoteControl string

//go:embed screen-cast/001_sessions.sql
var ScreenCast string
