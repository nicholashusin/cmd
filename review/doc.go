// Review provides a local code review tool for Go code in Jujutsu (jj) and Git
// repositories. The intent is for humans to interact with it via the web UI,
// and agents via the CLI.
//
// This allows two main workflows:
//
//  1. Humans use the web UI to review and leave comments on agent-generated
//     code. Agents use the CLI to read the comments and subsequently address
//     them.
//
//  2. Humans ask agents to review their code and leave comments using the
//     CLI. The comments left by the agents can then be viewed in a nicer
//     traditional code review format by the humans.
//
// To get started, run review in any jj / Git repository. Letting agents know
// that the binary exists so they can figure out how to use it themselves
// should be sufficient.
package main
