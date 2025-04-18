// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1 "github.com/openshift/api/image/v1"
	imagev1 "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	gentype "k8s.io/client-go/gentype"
)

// fakeImageTags implements ImageTagInterface
type fakeImageTags struct {
	*gentype.FakeClientWithList[*v1.ImageTag, *v1.ImageTagList]
	Fake *FakeImageV1
}

func newFakeImageTags(fake *FakeImageV1, namespace string) imagev1.ImageTagInterface {
	return &fakeImageTags{
		gentype.NewFakeClientWithList[*v1.ImageTag, *v1.ImageTagList](
			fake.Fake,
			namespace,
			v1.SchemeGroupVersion.WithResource("imagetags"),
			v1.SchemeGroupVersion.WithKind("ImageTag"),
			func() *v1.ImageTag { return &v1.ImageTag{} },
			func() *v1.ImageTagList { return &v1.ImageTagList{} },
			func(dst, src *v1.ImageTagList) { dst.ListMeta = src.ListMeta },
			func(list *v1.ImageTagList) []*v1.ImageTag { return gentype.ToPointerSlice(list.Items) },
			func(list *v1.ImageTagList, items []*v1.ImageTag) { list.Items = gentype.FromPointerSlice(items) },
		),
		fake,
	}
}
