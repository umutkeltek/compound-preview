package main

import (
	"fmt"
	"strings"

	"compound/internal/reviewartifact"
)

func (a synthesisSweepArtifact) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return operatorCardForSynthesisSweep(a)
}

func operatorCardForSynthesisSweep(a synthesisSweepArtifact) (reviewartifact.OperatorCard, error) {
	workID, err := reviewartifact.RequireText("work_id", a.WorkID, a.ID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	return reviewartifact.NewOperatorCard(
		"Memory synthesis finished sweep — proposals recorded, nothing applied. Pick at most one repair to open.",
		"memory synthesis",
		"synthesis-sweeps|"+workID,
		"memory repair debt stays bounded to one reviewed move",
	)
}

func (a synthesisContradictionArtifact) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return operatorCardForSynthesisContradiction(a)
}

func operatorCardForSynthesisContradiction(a synthesisContradictionArtifact) (reviewartifact.OperatorCard, error) {
	workID, err := reviewartifact.RequireText("work_id", a.WorkID, a.ID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	topic := synthesisClusterTopic(a.Proposal.ClusterKey, a.Proposal.Objects)
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Memory holds two claims about %s that disagree. Pick which is true?", topic),
		topic,
		"synthesis-contradictions|"+workID,
		"memory stops carrying both versions",
	)
}

func (a synthesisSeedCorroborationArtifact) ReviewOperatorCard() (reviewartifact.OperatorCard, error) {
	return operatorCardForSeedCorroboration(a)
}

func operatorCardForSeedCorroboration(a synthesisSeedCorroborationArtifact) (reviewartifact.OperatorCard, error) {
	candidate, err := reviewartifact.RequireText("seed candidate", a.CandidateID, a.Proposal.SeedCandidateID)
	if err != nil {
		return reviewartifact.OperatorCard{}, err
	}
	event := synthesisOperatorPlain(reviewartifact.FirstClause(a.Proposal.EvidenceSummary), 54)
	if event == "" {
		event = "a seed claim"
	}
	context := synthesisOperatorPlain(reviewartifact.HumanizeEntity(candidate), 56)
	if context == "" {
		context = "seed claim"
	}
	return reviewartifact.NewOperatorCard(
		fmt.Sprintf("Memory found a seed claim mattered later: %s. Review promotion?", event),
		context,
		"seed-corroborations|"+candidate,
		"memory gains only reviewed corroborated seeds",
	)
}

func synthesisClusterTopic(clusterKey string, objects []string) string {
	if len(objects) > 0 && strings.TrimSpace(objects[0]) != "" {
		return synthesisOperatorPlain(objects[0], 56)
	}
	parts := strings.FieldsFunc(clusterKey, func(r rune) bool {
		return r == '\x1f' || r == '|' || r == '/' || r == ':'
	})
	for i := range parts {
		parts[i] = synthesisOperatorPlain(parts[i], 56)
	}
	return reviewartifact.ShortLabel(reviewartifact.FirstNonEmpty(strings.Join(parts, " · "), "claim group"), 56)
}

func synthesisOperatorPlain(value string, max int) string {
	value = strings.NewReplacer(
		"=", " ",
		"/", " ",
		"`", " ",
		"\"", " ",
	).Replace(value)
	return reviewartifact.ShortLabel(reviewartifact.PlainLabel(value), max)
}
