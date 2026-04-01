package drivers

import "testing"

func TestIsExpectedIncusDockerRemote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		remote incusRemote
		want   bool
	}{
		{
			name: "matching",
			remote: incusRemote{
				Name:     incusDockerRemote,
				Protocol: incusDockerRemoteProtocol,
				URL:      incusDockerRemoteURL,
			},
			want: true,
		},
		{
			name: "matching short url",
			remote: incusRemote{
				Name:     incusDockerRemote,
				Protocol: incusDockerRemoteProtocol,
				URL:      "docker.io",
			},
			want: true,
		},
		{
			name: "protocol mismatch",
			remote: incusRemote{
				Name:     incusDockerRemote,
				Protocol: "simplestreams",
				URL:      incusDockerRemoteURL,
			},
			want: false,
		},
		{
			name: "url mismatch",
			remote: incusRemote{
				Name:     incusDockerRemote,
				Protocol: incusDockerRemoteProtocol,
				URL:      "https://example.com",
			},
			want: false,
		},
		{
			name: "different name",
			remote: incusRemote{
				Name:     "other",
				Protocol: incusDockerRemoteProtocol,
				URL:      incusDockerRemoteURL,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isExpectedIncusDockerRemote(tc.remote); got != tc.want {
				t.Fatalf("isExpectedIncusDockerRemote() = %v, want %v", got, tc.want)
			}
		})
	}
}

