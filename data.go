package main

type PullRequestEvent struct {
	Action      string `codec:"action"`
	PullRequest struct {
		HTMLURL string `codec:"html_url"`
		Head    struct {
			SHA   string `codec:"sha"`
			Owner struct {
				Login string `codec:"login"`
			} `codec:"user"`
			Repository struct {
				Name     string `codec:"name"`
				CloneURL string `codec:"clone_url"`
			} `codec:"repo"`
		} `codec:"head"`
	} `codec:"pull_request"`
}

type BuildEnqueuedEvent struct {
	PullRequest map[string]interface{} `codec:"pull_request"`
}

type BuildFinishedEvent struct {
	Result      string                 `codec:"result"`
	PullRequest map[string]interface{} `codec:"pull_request"`
	OutputURL   string                 `codec:"output_url,omitempty"`
	Error       string                 `codec:"error,omitempty"`
}
