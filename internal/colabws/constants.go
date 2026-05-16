// Portions of this file are based on googlecolab/colab-mcp,
// licensed under the Apache License, Version 2.0.
// This file has been adapted for the Go implementation.

package colabws

const (
	ColabBaseURL         = "https://colab.research.google.com"
	ColabAlternativeURL  = "https://colab.google.com"
	ScratchPath          = "/notebooks/empty.ipynb"
	Subprotocol          = "mcp"
	BusyCloseCode        = 1013
	BusyCloseReason      = "Server is busy"
	accessTokenQueryName = "access_token"
)
