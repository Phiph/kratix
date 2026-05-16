package main

// slackPayload is what Slack's incoming-webhook endpoint accepts.
// We bypass the slack-go SDK on purpose: incoming webhooks take JSON,
// and we only need the Block Kit subset for an approval notification.
type slackPayload struct {
	Text   string       `json:"text"`   // fallback for clients that can't render blocks
	Blocks []slackBlock `json:"blocks"` // primary rendered content
}

type slackBlock struct {
	Type   string       `json:"type"`
	Text   *slackText   `json:"text,omitempty"`
	Fields []slackText  `json:"fields,omitempty"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toSlack converts the generic envelope into a Slack Block Kit payload.
// Same shape, different framing — Slack-native clients get rich
// formatting; everything else can use the generic envelope unchanged.
func toSlack(env genericEnvelope) slackPayload {
	return slackPayload{
		Text: "Agent proposal: " + env.Action + " on " + env.Subject,
		Blocks: []slackBlock{
			{
				Type: "header",
				Text: &slackText{Type: "plain_text", Text: "🔐 Agent proposal awaiting approval"},
			},
			{
				Type: "section",
				Fields: []slackText{
					{Type: "mrkdwn", Text: "*Action*\n" + env.Action},
					{Type: "mrkdwn", Text: "*Subject*\n`" + env.Subject + "`"},
					{Type: "mrkdwn", Text: "*Actor*\n`" + env.Actor + "`"},
					{Type: "mrkdwn", Text: "*Expires*\n" + env.ExpiresIn},
				},
			},
			{
				Type: "section",
				Text: &slackText{Type: "mrkdwn", Text: "*Rationale*\n" + env.Rationale},
			},
			{
				Type: "section",
				Text: &slackText{Type: "mrkdwn", Text: "*Approve*\n```" + env.ApproveCmd + "```"},
			},
			{
				Type: "context",
				Fields: []slackText{
					{Type: "mrkdwn", Text: "proposalId: `" + env.ProposalID + "`"},
					{Type: "mrkdwn", Text: "correlationId: `" + env.CorrelationID + "`"},
				},
			},
		},
	}
}
