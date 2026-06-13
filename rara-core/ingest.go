// ingest.go — Phase 1 deliverable #2: populate the `items` spine from the existing
// YouTube domain tables (channel_videos from harvest, playlist_videos from shelf).
//
// The spine is materialized, not a UNION view: one lightweight items row per discovered
// video, upserted idempotently on the natural key (lane, source_ref) where
// source_ref = youtube_video_id. Re-running ingest converges — already-known videos
// collapse onto their existing row (id stable, status untouched), new ones are added in
// `discovered`. flow_version is stamped here so an item finishes on the flow shape it
// started with, even if the flow is later edited.
//
// Reading the domain tables crosses the agent boundary, but only as a SELECT — there is
// no FK and no write, honouring the 1.0 isolation convention. The read sits behind the
// SpineSource seam so the ingest logic is exercised in tests with zero I/O.
package main

import (
	"context"
	"fmt"
)

// YouTubeVideo is the minimal projection the spine needs from a collected video. Title is
// carried for the Phase 3 metadata gate (gate_barato); Phase 1 only requires VideoID.
type YouTubeVideo struct {
	VideoID string // youtube_video_id -> items.source_ref
	Title   string
}

// SpineSource reads the collected-video universe the spine is built from. The concrete
// implementation UNIONs channel_videos and playlist_videos and dedups on youtube_video_id
// (a video may sit in many playlists and also be a channel upload — it is one item).
type SpineSource interface {
	YouTubeVideos(ctx context.Context) ([]YouTubeVideo, error)
}

// IngestYouTube upserts one `items` row per collected YouTube video. It returns the
// number of videos processed (created or refreshed). It does NOT create item_steps —
// materializing the per-item state-rows is the reconciler's job (single writer of the
// runtime spine), keeping discovery and orchestration cleanly separated.
func IngestYouTube(ctx context.Context, db Database, src SpineSource) (int, error) {
	flow, found, err := db.GetFlow(ctx, youtubeFlowName)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("ingest: flow %q not seeded (run SeedYouTubeLane first)", youtubeFlowName)
	}

	videos, err := src.YouTubeVideos(ctx)
	if err != nil {
		return 0, err
	}

	n := 0
	for _, v := range videos {
		if v.VideoID == "" {
			continue // skip malformed rows (e.g. a private/deleted playlist entry)
		}
		// DiscoverItem is idempotent on (lane, source_ref): new videos land in
		// `discovered` stamped with the CURRENT flow_version; re-discovered ones keep their
		// id, their in-flight status (runtime status is the reconciler's to write, never
		// ingest's) AND their original flow_version frozen at first discovery — so a flow
		// edit only reaches items discovered after it (in-flight items finish on the old
		// version).
		if _, err := db.DiscoverItem(ctx, Item{
			Lane:        laneYouTube,
			SourceRef:   v.VideoID,
			FlowID:      flow.ID,
			FlowVersion: flow.Version,
			Status:      itemDiscovered,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// PodcastEpisode is the minimal projection the spine needs from a collected podcast episode.
// GUID is the RSS item's stable id -> items.source_ref; Title is carried for symmetry (the
// gate reads metadata from podcast_episodes directly).
type PodcastEpisode struct {
	GUID  string
	Title string
}

// PodcastSource reads the collected-episode universe the podcast spine is built from. The
// concrete implementation reads podcast_episodes (written by the rara-podcast collector).
type PodcastSource interface {
	PodcastEpisodes(ctx context.Context) ([]PodcastEpisode, error)
}

// IngestPodcast upserts one `items` row per collected podcast episode (lane=podcast,
// source_ref=guid, public). Mirrors IngestYouTube: idempotent on (lane, source_ref), stamps
// the podcast flow's version, never regresses an in-flight item. Returns the count processed.
func IngestPodcast(ctx context.Context, db Database, src PodcastSource) (int, error) {
	flow, found, err := db.GetFlow(ctx, podcastFlowName)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("ingest: flow %q not seeded (run SeedPodcastLane first)", podcastFlowName)
	}
	episodes, err := src.PodcastEpisodes(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range episodes {
		if e.GUID == "" {
			continue // skip malformed rows (an episode with no stable id)
		}
		if _, err := db.DiscoverItem(ctx, Item{
			Lane:        lanePodcast,
			SourceRef:   e.GUID,
			FlowID:      flow.ID,
			FlowVersion: flow.Version,
			Status:      itemDiscovered,
			Sensitivity: sensitivityPublic,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// EmailItem is the minimal projection the spine needs from a collected email. MessageID is the
// Gmail message id -> items.source_ref; Subject is carried for symmetry (the gate reads
// metadata from the emails table directly).
type EmailItem struct {
	MessageID string
	Subject   string
}

// EmailSource reads the collected-email universe the email spine is built from. The concrete
// implementation reads the emails table (written by the rara-mail collector).
type EmailSource interface {
	Emails(ctx context.Context) ([]EmailItem, error)
}

// IngestEmail upserts one `items` row per collected email (lane=email, source_ref=message_id).
// Email content is PRIVATE, so every email item is stamped sensitivity=private — the router
// then keeps it off third-party models. Mirrors the other ingests: idempotent on
// (lane, source_ref), stamps the email flow's version, never regresses an in-flight item.
func IngestEmail(ctx context.Context, db Database, src EmailSource) (int, error) {
	flow, found, err := db.GetFlow(ctx, emailFlowName)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("ingest: flow %q not seeded (run SeedEmailLane first)", emailFlowName)
	}
	emails, err := src.Emails(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range emails {
		if e.MessageID == "" {
			continue // skip malformed rows (an email with no message id)
		}
		if _, err := db.DiscoverItem(ctx, Item{
			Lane:        laneEmail,
			SourceRef:   e.MessageID,
			FlowID:      flow.ID,
			FlowVersion: flow.Version,
			Status:      itemDiscovered,
			Sensitivity: sensitivityPrivate,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
