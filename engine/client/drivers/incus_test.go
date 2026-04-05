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
				Protocol: incusDockerRemoteProtocol,
				Addr:     incusDockerRemoteURL,
			},
			want: true,
		},
		{
			name: "matching short url",
			remote: incusRemote{
				Protocol: incusDockerRemoteProtocol,
				Addr:     "docker.io",
			},
			want: true,
		},
		{
			name: "protocol mismatch",
			remote: incusRemote{
				Protocol: "simplestreams",
				Addr:     incusDockerRemoteURL,
			},
			want: false,
		},
		{
			name: "url mismatch",
			remote: incusRemote{
				Protocol: incusDockerRemoteProtocol,
				Addr:     "https://example.com",
			},
			want: false,
		},
		{
			name: "different name",
			remote: incusRemote{
				Protocol: incusDockerRemoteProtocol,
				Addr:     incusDockerRemoteURL,
			},
			want: true,
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
