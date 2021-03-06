package cloud

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/roachprod/config"
	"github.com/cockroachdb/roachprod/vm"
	"github.com/pkg/errors"
)

const vmNameFormat = "user-<clusterid>-<nodeid>"

type Cloud struct {
	Clusters map[string]*CloudCluster `json:"clusters"`
	// Any VM in this list can be expected to have at least one element
	// in its Errors field.
	BadInstances vm.List `json:"bad_instances"`
}

// Collate Cloud.BadInstances by errors.
func (c *Cloud) BadInstanceErrors() map[error]vm.List {
	ret := map[error]vm.List{}

	// Expand instances and errors
	for _, vm := range c.BadInstances {
		for _, err := range vm.Errors {
			ret[err] = append(ret[err], vm)
		}
	}

	// Sort each List to make the output prettier
	for _, v := range ret {
		sort.Sort(v)
	}

	return ret
}

func newCloud() *Cloud {
	return &Cloud{
		Clusters: make(map[string]*CloudCluster),
	}
}

// A CloudCluster is created by querying various vm.Provider instances.
//
// TODO(benesch): unify with syncedCluster.
type CloudCluster struct {
	Name string `json:"name"`
	User string `json:"user"`
	// This is the earliest creation and shortest lifetime across VMs.
	CreatedAt time.Time     `json:"created_at"`
	Lifetime  time.Duration `json:"lifetime"`
	VMs       vm.List       `json:"vms"`
}

// Clouds returns the names of all of the various cloud providers used
// by the VMs in the cluster.
func (c *CloudCluster) Clouds() []string {
	present := make(map[string]bool)
	for _, m := range c.VMs {
		present[m.Provider] = true
	}

	var ret []string
	for provider := range present {
		ret = append(ret, provider)
	}
	sort.Strings(ret)
	return ret
}

func (c *CloudCluster) ExpiresAt() time.Time {
	return c.CreatedAt.Add(c.Lifetime)
}

func (c *CloudCluster) GCAt() time.Time {
	// NB: GC is performed every hour. We calculate the lifetime of the cluster
	// taking the GC time into account to accurately reflect when the cluster
	// will be destroyed.
	return c.ExpiresAt().Add(time.Hour - 1).Truncate(time.Hour)
}

func (c *CloudCluster) LifetimeRemaining() time.Duration {
	return time.Until(c.GCAt())
}

func (c *CloudCluster) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s: %d", c.Name, len(c.VMs))
	if !c.IsLocal() {
		fmt.Fprintf(&buf, " (%s)", c.LifetimeRemaining().Round(time.Second))
	}
	return buf.String()
}

func (c *CloudCluster) PrintDetails() {
	fmt.Printf("%s: %s ", c.Name, c.Clouds())
	if !c.IsLocal() {
		l := c.LifetimeRemaining().Round(time.Second)
		if l <= 0 {
			fmt.Printf("expired %s ago\n", -l)
		} else {
			fmt.Printf("%s remaining\n", l)
		}
	} else {
		fmt.Printf("(no expiration)\n")
	}
	for _, vm := range c.VMs {
		fmt.Printf("  %s\t%s\t%s\t%s\n", vm.Name, vm.DNS, vm.PrivateIP, vm.PublicIP)
	}
}

func (c *CloudCluster) IsLocal() bool {
	return c.Name == config.Local
}

func namesFromVM(v vm.VM) (string, string, error) {
	if v.IsLocal() {
		return config.Local, config.Local, nil
	}
	name := v.Name
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("expected VM name in the form %s, got %s", vmNameFormat, name)
	}
	return parts[0], strings.Join(parts[:len(parts)-1], "-"), nil
}

func ListCloud() (*Cloud, error) {
	cloud := newCloud()

	for _, p := range vm.Providers {
		vms, err := p.List()
		if err != nil {
			return nil, err
		}

		for _, v := range vms {
			// Parse cluster/user from VM name, but only for non-local VMs
			userName, clusterName, err := namesFromVM(v)
			if err != nil {
				v.Errors = append(v.Errors, vm.ErrInvalidName)
			}

			// Anything with an error gets tossed into the BadInstances slice, and we'll correct
			// the problem later on.
			if len(v.Errors) > 0 {
				cloud.BadInstances = append(cloud.BadInstances, v)
				continue
			}

			if _, ok := cloud.Clusters[clusterName]; !ok {
				cloud.Clusters[clusterName] = &CloudCluster{
					Name:      clusterName,
					User:      userName,
					CreatedAt: v.CreatedAt,
					Lifetime:  v.Lifetime,
					VMs:       nil,
				}
			}

			// Bound the cluster creation time and overall lifetime to the earliest and/or shortest VM
			c := cloud.Clusters[clusterName]
			c.VMs = append(c.VMs, v)
			if v.CreatedAt.Before(c.CreatedAt) {
				c.CreatedAt = v.CreatedAt
			}
			if v.Lifetime < c.Lifetime {
				c.Lifetime = v.Lifetime
			}
		}
	}

	// Sort VMs for each cluster. We want to make sure we always have the same order.
	for _, c := range cloud.Clusters {
		sort.Sort(c.VMs)
	}

	return cloud, nil
}

func CreateCluster(name string, nodes int, opts vm.CreateOpts) error {
	providerCount := len(opts.VMProviders)
	if providerCount == 0 {
		return errors.New("no VMProviders configured")
	}

	// Allocate vm names over the configured providers
	vmLocations := map[string][]string{}
	for i, p := 1, 0; i <= nodes; i++ {
		pName := opts.VMProviders[p]
		vmName := fmt.Sprintf("%s-%0.4d", name, i)
		vmLocations[pName] = append(vmLocations[pName], vmName)

		p = (p + 1) % providerCount
	}

	return vm.ProvidersParallel(opts.VMProviders, func(p vm.Provider) error {
		return p.Create(vmLocations[p.Name()], opts)
	})
}

func DestroyCluster(c *CloudCluster) error {
	return vm.FanOut(c.VMs, func(p vm.Provider, vms vm.List) error {
		return p.Delete(vms)
	})
}

func ExtendCluster(c *CloudCluster, extension time.Duration) error {
	newLifetime := c.Lifetime + extension

	return vm.FanOut(c.VMs, func(p vm.Provider, vms vm.List) error {
		return p.Extend(vms, newLifetime)
	})
}
