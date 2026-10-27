package migrations

import _ "embed"

//go:embed remote-control/001_sessions.sql
var RemoteControl string

//go:embed screen-cast/001_sessions.sql
var ScreenCast string

//go:embed live-studio/001_studio.sql
var LiveStudio string

//go:embed conference/001_meetings.sql
var Conference string
