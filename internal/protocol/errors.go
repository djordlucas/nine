package protocol

// ConflictPrefix opens the text of an error reply that refuses a request
// because of the target's state rather than the request's shape or the target's
// absence — deleting a goal that nine.toml declares, for one.
//
// Errors travel as strings (Msg.Text), so a client that wants to tell a
// conflict apart from a failure has only the text to go on. Fixing that prefix
// here, in one place both sides import, is what keeps the classification from
// depending on a sentence a later edit could reword.
const ConflictPrefix = "conflict: "
