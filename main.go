package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shurcooL/githubv4"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

type pullRequest struct {
	Title  githubv4.String
	Author struct {
		Login string
	}
	IsDraft   githubv4.Boolean
	CreatedAt githubv4.GitTimestamp
	Reviews   struct {
		TotalCount int
		Nodes      []struct {
			State       githubv4.PullRequestReviewState
			SubmittedAt githubv4.GitTimestamp
		}
	} `graphql:"reviews(first: 100)"`
}

var (
	pullRequestsCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "github",
		Subsystem: "gitpod_io",
		Name:      "pull_requests_count",
	}, []string{"state"})
)

func main() {
	githubToken := os.Getenv("GITHUB_TOKEN")
	if len(githubToken) == 0 {
		log.Fatal("missing GITHUB_TOKEN env var")
	}

	prometheus.MustRegister(pullRequestsCount)

	src := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: githubToken},
	)
	httpClient := oauth2.NewClient(context.Background(), src)
	githubClient := githubv4.NewClient(httpClient)
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()

		for {
			prs, err := getPullRequests(githubClient, "gitpod-io", "gitpod")
			if err != nil {
				log.WithError(err).Error("cannot download pull requests")
				continue
			}

			updateMetrics(prs)
			<-t.C
		}
	}()

	log.Info("serving metrics at :9500/metrics")

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":9500", nil)
}

func updateMetrics(prs []pullRequest) error {
	report := reportWIP(prs)
	pullRequestsCount.With(prometheus.Labels{
		"state": "draft",
	}).Set(float64(len(report.Draft)))
	pullRequestsCount.With(prometheus.Labels{
		"state": "approved",
	}).Set(float64(len(report.Approved)))
	pullRequestsCount.With(prometheus.Labels{
		"state": "overdue",
	}).Set(float64(len(report.OverdueReview)))
	pullRequestsCount.With(prometheus.Labels{
		"state": "commented",
	}).Set(float64(len(report.Commented)))
	return nil
}

func getPullRequests(client *githubv4.Client, owner, name string) ([]pullRequest, error) {
	type queryPR struct {
		Repository struct {
			PullRequests struct {
				Nodes    []pullRequest
				PageInfo struct {
					EndCursor   githubv4.String
					HasNextPage bool
				}
			} `graphql:"pullRequests(states: OPEN, first: 100, after:$prCursor)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}

	vars := map[string]interface{}{
		"owner":    githubv4.String("gitpod-io"),
		"name":     githubv4.String("gitpod"),
		"prCursor": (*githubv4.String)(nil),
	}

	var response []pullRequest
	for {
		var q queryPR
		err := client.Query(context.Background(), &q, vars)
		if err != nil {
			return nil, fmt.Errorf("cannot query GitHub: %v", err)
		}
		response = append(response, q.Repository.PullRequests.Nodes...)

		if !q.Repository.PullRequests.PageInfo.HasNextPage {
			break
		}
		vars["prCursor"] = q.Repository.PullRequests.PageInfo.EndCursor
	}
	return response, nil
}

type wipReport struct {
	Open          []*pullRequest
	Draft         []*pullRequest
	Approved      []*pullRequest
	Commented     []*pullRequest
	OverdueReview []*pullRequest
}

func reportWIP(prs []pullRequest) wipReport {
	var res wipReport
	for _, pr := range prs {
		pr := pr
		res.Open = append(res.Open, &pr)

		if pr.IsDraft {
			res.Draft = append(res.Draft, &pr)
			continue
		}

		var (
			lastComment time.Time
			approved    bool
		)
		for _, review := range pr.Reviews.Nodes {
			if review.State == githubv4.PullRequestReviewStateApproved {
				approved = true
			}
			if review.State == githubv4.PullRequestReviewStateCommented {
				res.Commented = append(res.Commented, &pr)
				if lastComment.Before(review.SubmittedAt.Time) {
					lastComment = review.SubmittedAt.Time
				}
			}
		}
		if approved {
			res.Approved = append(res.Approved, &pr)
		} else if (lastComment.IsZero() && time.Since(pr.CreatedAt.Time) > 24*time.Hour) || (time.Since(lastComment) > 24*time.Hour) {
			res.OverdueReview = append(res.OverdueReview, &pr)
		}
	}
	return res
}

func printReport(out io.Writer, r wipReport) {
	w := &tabwriter.Writer{}
	w.Init(out, 10, 4, 0, ' ', 0)
	defer w.Flush()

	fmt.Fprintf(w, "Open:\t%d\n", len(r.Open))
	fmt.Fprintf(w, "Approved:\t%d\n", len(r.Approved))
	fmt.Fprintf(w, "Commented:\t%d\n", len(r.Commented))
	fmt.Fprintf(w, "Overdue:\t%d\n", len(r.OverdueReview))
}
