package libimage

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	filtersPkg "github.com/containers/common/pkg/filters"
	"github.com/containers/common/pkg/timetype"
	"github.com/containers/image/v5/docker/reference"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// filterFunc is a prototype for a positive image filter.  Returning `true`
// indicates that the image matches the criteria.
type filterFunc func(*Image) (bool, error)

// filterImages returns a slice of images which are passing all specified
// filters.
func (r *Runtime) filterImages(ctx context.Context, images []*Image, options *ListImagesOptions) ([]*Image, error) {
	if len(options.Filters) == 0 || len(images) == 0 {
		return images, nil
	}

	disjoints, filters, err := r.compileImageFilters(ctx, options)
	if err != nil {
		return nil, err
	}
	result := []*Image{}
	for i := range images {
		include := false
		var err error
		for _, filter := range disjoints {
			include, err = filter(images[i])
			if err != nil {
				return nil, err
			}
		}
		for _, filter := range filters {
			include, err = filter(images[i])
			if err != nil {
				return nil, err
			}
			if !include {
				break
			}
		}
		if include {
			result = append(result, images[i])
		}
	}
	return result, nil
}

// compileImageFilters creates `filterFunc`s for the specified filters.  The
// required format is `key=value` with the following supported keys:
//           after, since, before, containers, dangling, id, label, readonly, reference, intermediate
func (r *Runtime) compileImageFilters(ctx context.Context, options *ListImagesOptions) ([]filterFunc, []filterFunc, error) {
	logrus.Tracef("Parsing image filters %s", options.Filters)

	var tree *layerTree
	getTree := func() (*layerTree, error) {
		if tree == nil {
			t, err := r.layerTree()
			if err != nil {
				return nil, err
			}
			tree = t
		}
		return tree, nil
	}

	filterFuncs := []filterFunc{}
	disjointFuncs := []filterFunc{}
	for _, filter := range options.Filters {
		var key, value string
		split := strings.SplitN(filter, "=", 2)
		if len(split) != 2 {
			return nil, nil, errors.Errorf("invalid image filter %q: must be in the format %q", filter, "filter=value")
		}

		key = split[0]
		value = split[1]
		switch key {

		case "after", "since":
			img, _, err := r.LookupImage(value, nil)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "could not find local image for filter %q", filter)
			}
			filterFuncs = append(filterFuncs, filterAfter(img.Created()))

		case "before":
			img, _, err := r.LookupImage(value, nil)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "could not find local image for filter %q", filter)
			}
			filterFuncs = append(filterFuncs, filterBefore(img.Created()))

		case "containers":
			switch value {
			case "false", "true":
			case "external":
				if options.IsExternalContainerFunc == nil {
					return nil, nil, fmt.Errorf("libimage error: external containers filter without callback")
				}
			default:
				return nil, nil, fmt.Errorf("unsupported value %q for containers filter", value)
			}
			filterFuncs = append(filterFuncs, filterContainers(value, options.IsExternalContainerFunc))

		case "dangling":
			dangling, err := strconv.ParseBool(value)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "non-boolean value %q for dangling filter", value)
			}
			t, err := getTree()
			if err != nil {
				return nil, nil, err
			}
			filterFuncs = append(filterFuncs, filterDangling(ctx, dangling, t))

		case "id":
			filterFuncs = append(filterFuncs, filterID(value))

		case "intermediate":
			intermediate, err := strconv.ParseBool(value)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "non-boolean value %q for intermediate filter", value)
			}
			t, err := getTree()
			if err != nil {
				return nil, nil, err
			}
			filterFuncs = append(filterFuncs, filterIntermediate(ctx, intermediate, t))

		case "label":
			filterFuncs = append(filterFuncs, filterLabel(ctx, value))

		case "readonly":
			readOnly, err := strconv.ParseBool(value)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "non-boolean value %q for readonly filter", value)
			}
			filterFuncs = append(filterFuncs, filterReadOnly(readOnly))

		case "reference":
			disjointFuncs = append(disjointFuncs, filterReferences(value))

		case "until":
			ts, err := timetype.GetTimestamp(value, time.Now())
			if err != nil {
				return nil, nil, err
			}
			seconds, nanoseconds, err := timetype.ParseTimestamps(ts, 0)
			if err != nil {
				return nil, nil, err
			}
			until := time.Unix(seconds, nanoseconds)
			filterFuncs = append(filterFuncs, filterBefore(until))

		default:
			return nil, nil, errors.Errorf("unsupported image filter %q", key)
		}
	}

	return disjointFuncs, filterFuncs, nil
}

// filterReferences creates a reference filter for matching the specified value.
func filterReferences(value string) filterFunc {
	return func(img *Image) (bool, error) {
		refs, err := img.NamesReferences()
		if err != nil {
			return false, err
		}

		for _, ref := range refs {
			refString := ref.String() // FQN with tag/digest
			candidates := []string{refString}

			// Split the reference into 3 components (twice if diggested/tagged):
			// 1) Fully-qualified reference
			// 2) Without domain
			// 3) Without domain and path
			if named, isNamed := ref.(reference.Named); isNamed {
				candidates = append(candidates,
					reference.Path(named),                           // path/name without tag/digest (Path() removes it)
					refString[strings.LastIndex(refString, "/")+1:]) // name with tag/digest

				trimmedString := reference.TrimNamed(named).String()
				if refString != trimmedString {
					tagOrDigest := refString[len(trimmedString):]
					candidates = append(candidates,
						trimmedString,                     // FQN without tag/digest
						reference.Path(named)+tagOrDigest, // path/name with tag/digest
						trimmedString[strings.LastIndex(trimmedString, "/")+1:]) // name without tag/digest
				}
			}

			for _, candidate := range candidates {
				// path.Match() is also used by Docker's reference.FamiliarMatch().
				matched, _ := path.Match(value, candidate)
				if matched {
					return true, nil
				}
			}
		}
		return false, nil
	}
}

// filterLabel creates a label for matching the specified value.
func filterLabel(ctx context.Context, value string) filterFunc {
	return func(img *Image) (bool, error) {
		labels, err := img.Labels(ctx)
		if err != nil {
			return false, err
		}
		return filtersPkg.MatchLabelFilters([]string{value}, labels), nil
	}
}

// filterAfter creates an after filter for matching the specified value.
func filterAfter(value time.Time) filterFunc {
	return func(img *Image) (bool, error) {
		return img.Created().After(value), nil
	}
}

// filterBefore creates a before filter for matching the specified value.
func filterBefore(value time.Time) filterFunc {
	return func(img *Image) (bool, error) {
		return img.Created().Before(value), nil
	}
}

// filterReadOnly creates a readonly filter for matching the specified value.
func filterReadOnly(value bool) filterFunc {
	return func(img *Image) (bool, error) {
		return img.IsReadOnly() == value, nil
	}
}

// filterContainers creates a container filter for matching the specified value.
func filterContainers(value string, fn IsExternalContainerFunc) filterFunc {
	return func(img *Image) (bool, error) {
		ctrs, err := img.Containers()
		if err != nil {
			return false, err
		}
		if value != "external" {
			boolValue := value == "true"
			return (len(ctrs) > 0) == boolValue, nil
		}

		// Check whether all associated containers are external ones.
		for _, c := range ctrs {
			isExternal, err := fn(c)
			if err != nil {
				return false, fmt.Errorf("checking if %s is an external container in filter: %w", c, err)
			}
			if !isExternal {
				return isExternal, nil
			}
		}
		return true, nil
	}
}

// filterDangling creates a dangling filter for matching the specified value.
func filterDangling(ctx context.Context, value bool, tree *layerTree) filterFunc {
	return func(img *Image) (bool, error) {
		isDangling, err := img.isDangling(ctx, tree)
		if err != nil {
			return false, err
		}
		return isDangling == value, nil
	}
}

// filterID creates an image-ID filter for matching the specified value.
func filterID(value string) filterFunc {
	return func(img *Image) (bool, error) {
		return img.ID() == value, nil
	}
}

// filterIntermediate creates an intermediate filter for images.  An image is
// considered to be an intermediate image if it is dangling (i.e., no tags) and
// has no children (i.e., no other image depends on it).
func filterIntermediate(ctx context.Context, value bool, tree *layerTree) filterFunc {
	return func(img *Image) (bool, error) {
		isIntermediate, err := img.isIntermediate(ctx, tree)
		if err != nil {
			return false, err
		}
		return isIntermediate == value, nil
	}
}
