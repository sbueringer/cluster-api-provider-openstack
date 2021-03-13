module sigs.k8s.io/cluster-api-provider-openstack/hack/tools

go 1.16

require (
	github.com/a8m/envsubst v1.2.0
	github.com/go-openapi/spec v0.19.5 // indirect
	github.com/golang/mock v1.4.4
	github.com/golangci/golangci-lint v1.27.0
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-runewidth v0.0.9 // indirect
	github.com/niemeyer/pretty v0.0.0-20200227124842-a10e7caefd8e // indirect
	github.com/onsi/ginkgo v1.15.0
	golang.org/x/sys v0.0.0-20210113181707-4bcb84eeeb78 // indirect
	gopkg.in/check.v1 v1.0.0-20200227125254-8fa46927fb4f // indirect
	gopkg.in/yaml.v3 v3.0.0-20210107192922-496545a6307b // indirect
	k8s.io/code-generator v0.21.0-beta.0
	sigs.k8s.io/cluster-api v0.3.11-0.20210305093021-046ab290ba3c
	sigs.k8s.io/cluster-api/hack/tools v0.0.0-20210305093021-046ab290ba3c
	sigs.k8s.io/controller-tools v0.5.0
	sigs.k8s.io/kind v0.9.0
	sigs.k8s.io/testing_frameworks v0.1.2
)

// pin for now to avoid fixing all the linter issues in the current PR
// TODO(sbueringer): upgrade to current linter and fix the occuring issues
replace github.com/golangci/golangci-lint => github.com/golangci/golangci-lint v1.23.8
