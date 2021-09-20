package v1

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/grpcutil"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/test/bufconn"

	"github.com/authzed/spicedb/internal/datastore/memdb"
	"github.com/authzed/spicedb/internal/dispatch/graph"
	"github.com/authzed/spicedb/internal/namespace"
	tf "github.com/authzed/spicedb/internal/testfixtures"
	"github.com/authzed/spicedb/pkg/zedtoken"
)

var testTimedeltas = []time.Duration{0, 1 * time.Second}

func init() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	// Set this to Trace to dump log statements in tests.
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
}

func sub(subType string, subID string, subRel string) *v1.SubjectReference {
	return &v1.SubjectReference{
		Object: &v1.ObjectReference{
			ObjectType: subType,
			ObjectId:   subID,
		},
		OptionalRelation: subRel,
	}
}

func TestLookupResources(t *testing.T) {
	testCases := []struct {
		objectType        string
		permission        string
		subject           *v1.SubjectReference
		expectedObjectIds []string
		expectedErrorCode codes.Code
	}{
		{
			"document", "viewer",
			sub("user", "eng_lead", ""),
			[]string{"masterplan"},
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "product_manager", ""),
			[]string{"masterplan"},
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "chief_financial_officer", ""),
			[]string{"masterplan", "healthplan"},
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "auditor", ""),
			[]string{"masterplan", "companyplan"},
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "vp_product", ""),
			[]string{"masterplan"},
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "legal", ""),
			[]string{"masterplan", "companyplan"},
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "owner", ""),
			[]string{"masterplan", "companyplan"},
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "villain", ""),
			nil,
			codes.OK,
		},
		{
			"document", "viewer",
			sub("user", "unknowngal", ""),
			nil,
			codes.OK,
		},

		{
			"document", "viewer_and_editor",
			sub("user", "eng_lead", ""),
			nil,
			codes.OK,
		},
		{
			"document", "viewer_and_editor",
			sub("user", "multiroleguy", ""),
			[]string{"specialplan"},
			codes.OK,
		},
		{
			"document", "viewer_and_editor",
			sub("user", "missingrolegal", ""),
			nil,
			codes.OK,
		},
		{
			"document", "viewer_and_editor_derived",
			sub("user", "multiroleguy", ""),
			[]string{"specialplan"},
			codes.OK,
		},
		{
			"document", "viewer_and_editor_derived",
			sub("user", "missingrolegal", ""),
			nil,
			codes.OK,
		},
		{
			"document", "invalidrelation",
			sub("user", "missingrolegal", ""),
			[]string{},
			codes.FailedPrecondition,
		},
		{
			"document", "viewer_and_editor_derived",
			sub("user", "someuser", "invalidrelation"),
			[]string{},
			codes.FailedPrecondition,
		},
		{
			"invalidnamespace", "viewer_and_editor_derived",
			sub("user", "someuser", ""),
			[]string{},
			codes.FailedPrecondition,
		},
		{
			"document", "viewer_and_editor_derived",
			sub("invalidnamespace", "someuser", ""),
			[]string{},
			codes.FailedPrecondition,
		},
	}

	for _, delta := range testTimedeltas {
		t.Run(fmt.Sprintf("fuzz%d", delta/time.Millisecond), func(t *testing.T) {
			for _, tc := range testCases {
				t.Run(fmt.Sprintf("%s::%s from %s:%s#%s", tc.objectType, tc.permission, tc.subject.Object.ObjectType, tc.subject.Object.ObjectId, tc.subject.OptionalRelation), func(t *testing.T) {
					require := require.New(t)
					client, stop, revision := newPermissionsServicer(require, delta, memdb.DisableGC, 0)
					defer stop()

					lookupClient, err := client.LookupResources(context.Background(), &v1.LookupResourcesRequest{
						ResourceObjectType: tc.objectType,
						Permission:         tc.permission,
						Subject:            tc.subject,
						Consistency: &v1.Consistency{
							Requirement: &v1.Consistency_AtLeastAsFresh{
								AtLeastAsFresh: zedtoken.NewFromRevision(revision),
							},
						},
					})

					require.NoError(err)
					if tc.expectedErrorCode == codes.OK {
						var resolvedObjectIds []string
						for {
							resp, err := lookupClient.Recv()
							if err == io.EOF {
								break
							}

							require.NoError(err)

							resolvedObjectIds = append(resolvedObjectIds, resp.ResourceObjectId)
						}

						sort.Strings(tc.expectedObjectIds)
						sort.Strings(resolvedObjectIds)

						require.Equal(tc.expectedObjectIds, resolvedObjectIds)
					} else {
						_, err := lookupClient.Recv()
						grpcutil.RequireStatus(t, tc.expectedErrorCode, err)
					}
				})
			}
		})
	}
}

func newPermissionsServicer(
	require *require.Assertions,
	revisionFuzzingTimedelta time.Duration,
	gcWindow time.Duration,
	simulatedLatency time.Duration,
) (v1.PermissionsServiceClient, func(), decimal.Decimal) {
	emptyDS, err := memdb.NewMemdbDatastore(0, revisionFuzzingTimedelta, gcWindow, simulatedLatency)
	require.NoError(err)

	ds, revision := tf.StandardDatastoreWithData(emptyDS, require)

	ns, err := namespace.NewCachingNamespaceManager(ds, 1*time.Second, nil)
	require.NoError(err)

	dispatch := graph.NewLocalOnlyDispatcher(ns, ds)
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	RegisterPermissionsServer(s, ds, ns, dispatch, 50)
	go s.Serve(lis)

	conn, err := grpc.Dial("", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return lis.Dial()
	}), grpc.WithInsecure())
	require.NoError(err)

	return v1.NewPermissionsServiceClient(conn), func() {
		s.Stop()
		lis.Close()
	}, revision
}