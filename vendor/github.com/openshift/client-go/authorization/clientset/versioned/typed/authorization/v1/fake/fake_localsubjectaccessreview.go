// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	context "context"

	v1 "github.com/openshift/api/authorization/v1"
	authorizationv1 "github.com/openshift/client-go/authorization/clientset/versioned/typed/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gentype "k8s.io/client-go/gentype"
	testing "k8s.io/client-go/testing"
)

// fakeLocalSubjectAccessReviews implements LocalSubjectAccessReviewInterface
type fakeLocalSubjectAccessReviews struct {
	*gentype.FakeClient[*v1.LocalSubjectAccessReview]
	Fake *FakeAuthorizationV1
}

func newFakeLocalSubjectAccessReviews(fake *FakeAuthorizationV1, namespace string) authorizationv1.LocalSubjectAccessReviewInterface {
	return &fakeLocalSubjectAccessReviews{
		gentype.NewFakeClient[*v1.LocalSubjectAccessReview](
			fake.Fake,
			namespace,
			v1.SchemeGroupVersion.WithResource("localsubjectaccessreviews"),
			v1.SchemeGroupVersion.WithKind("LocalSubjectAccessReview"),
			func() *v1.LocalSubjectAccessReview { return &v1.LocalSubjectAccessReview{} },
		),
		fake,
	}
}

// Create takes the representation of a localSubjectAccessReview and creates it.  Returns the server's representation of the subjectAccessReviewResponse, and an error, if there is any.
func (c *fakeLocalSubjectAccessReviews) Create(ctx context.Context, localSubjectAccessReview *v1.LocalSubjectAccessReview, opts metav1.CreateOptions) (result *v1.SubjectAccessReviewResponse, err error) {
	emptyResult := &v1.SubjectAccessReviewResponse{}
	obj, err := c.Fake.
		Invokes(testing.NewCreateActionWithOptions(c.Resource(), c.Namespace(), localSubjectAccessReview, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.SubjectAccessReviewResponse), err
}
