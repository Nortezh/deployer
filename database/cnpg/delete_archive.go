package cnpg

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/deploys-app/deployer/k8s"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const deleteMinIOProfile = "minio-native-gc-v1"

func validateDeleteArchive(ctx context.Context, c *k8s.Client, p Profile, inv *deleteInventory) error {
	b := &inv.Binding
	if p.ArchiveDeleteProvider != deleteMinIOProfile || b.Namespace != c.DBNamespace() || p.StorageClass != b.StorageClass || p.BackupObjectStore != b.ObjectStoreName || p.BackupProfileRevision != b.Archive.ProfileRevision {
		return errDeleteBlocked
	}
	store, err := c.Dynamic().Resource(objectStoreGVR).Namespace(c.DBNamespace()).Get(ctx, b.ObjectStoreName, metav1.GetOptions{})
	if err != nil || string(store.GetUID()) != b.ObjectStoreUID || store.GetGeneration() != b.ObjectStoreGeneration {
		return errDeleteBlocked
	}
	profile := p
	profile.Image = b.Image // current create-image settings are not a deletion prerequisite.
	archive, err := backupArchive(ctx, c, profile, store)
	if err != nil || !reflect.DeepEqual(archive, &b.Archive) {
		return errDeleteBlocked
	}
	_, secrets, _, supported := readinessStore(ctx, c, profile, store)
	selectors, _, _ := unstructured.NestedMap(store.Object, "spec", "configuration", "s3Credentials")
	if !supported || len(b.Credentials) != 2 {
		return errDeleteBlocked
	}
	for i, role := range []string{"accessKeyId", "secretAccessKey"} {
		selector := selectors[role].(map[string]any)
		expected := readinessCredential{Name: secrets[i].Name, Key: selector["key"].(string), UID: string(secrets[i].UID), ResourceVersion: secrets[i].ResourceVersion}
		if b.Credentials[i] != expected {
			return errDeleteBlocked
		}
	}
	if _, err := deleteRoot(ctx, c, inv); err != nil {
		return err
	}
	if exclusiveArchiveScope(ctx, c, b, inv.Objects[0].UID) != nil {
		return errDeleteBlocked
	}
	// Shared credentials remain in place; only their exact originally bound selectors are read.
	for _, ref := range b.Credentials {
		secret, err := c.Core().CoreV1().Secrets(c.DBNamespace()).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil || secret.DeletionTimestamp != nil || string(secret.UID) != ref.UID || secret.ResourceVersion != ref.ResourceVersion || len(secret.Data[ref.Key]) == 0 {
			return errDeleteBlocked
		}
	}
	app, err := c.Core().CoreV1().Secrets(c.DBNamespace()).Get(ctx, b.Server+"-app", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) && (err != nil || string(app.UID) != b.BootstrapUID) {
		return errDeleteBlocked
	}
	return nil
}

func archiveDeleteClient(ctx context.Context, c *k8s.Client, b *archiveBinding) (*s3.Client, error) {
	if len(b.Credentials) != 2 {
		return nil, errDeleteBlocked
	}
	values := make([]string, 2)
	for i, ref := range b.Credentials {
		secret, err := c.Core().CoreV1().Secrets(c.DBNamespace()).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil || string(secret.UID) != ref.UID || secret.ResourceVersion != ref.ResourceVersion || secret.DeletionTimestamp != nil || len(secret.Data[ref.Key]) == 0 {
			return nil, errDeleteBlocked
		}
		values[i] = string(secret.Data[ref.Key])
	}
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(values[0], values[1], ""), RetryMaxAttempts: 1,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, func(o *s3.Options) { o.BaseEndpoint = aws.String(b.Archive.EndpointURL); o.UsePathStyle = true })
	return client, nil
}

func archiveAPIError(err error, code string) bool {
	var apiError smithy.APIError
	return errors.As(err, &apiError) && apiError.ErrorCode() == code
}

// An explicit reviewed MinIO profile is necessary but not sufficient: denied/unknown bucket features block.
func checkDeleteBucket(ctx context.Context, client *s3.Client, bucket string) error {
	versioning, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &bucket})
	if err != nil || versioning == nil || versioning.Status != "" || versioning.MFADelete != "" {
		return errDeleteBlocked
	}
	_, err = client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: &bucket})
	if !archiveAPIError(err, "ObjectLockConfigurationNotFoundError") {
		return errDeleteBlocked
	}
	_, err = client.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: &bucket})
	if !archiveAPIError(err, "ReplicationConfigurationNotFoundError") {
		return errDeleteBlocked
	}
	return nil
}

func purgeDeleteArchive(ctx context.Context, c *k8s.Client, p Profile, inv *deleteInventory) error {
	b := &inv.Binding
	client, err := archiveDeleteClient(ctx, c, b)
	if err != nil || checkDeleteBucket(ctx, client, b.Bucket) != nil {
		return errDeleteBlocked
	}
	// shortcut: at most 128 pages per pass, larger scopes resume on the same operation's next poll.
	for _, remove := range []bool{true, false} {
		var token *string
		seenTokens := map[string]bool{}
		complete := false
		for page := 0; page < 128; page++ {
			if ctx.Err() != nil || validateDeleteArchive(ctx, c, p, inv) != nil || checkDeleteProfile(ctx, c, p, inv) != nil || checkDeleteDependents(ctx, c, b.Server, inv) != nil || verifyDeleteStopped(ctx, c, inv) != nil {
				return errDeleteBlocked
			}
			listed, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &b.Bucket, Prefix: &b.Prefix, ContinuationToken: token, MaxKeys: aws.Int32(1000)})
			if err != nil || listed == nil || listed.Prefix == nil || *listed.Prefix != b.Prefix || len(listed.CommonPrefixes) != 0 || len(listed.Contents) > 1000 || listed.Name == nil || *listed.Name != b.Bucket || listed.KeyCount == nil || int(*listed.KeyCount) != len(listed.Contents) {
				return errDeleteBlocked
			}
			keys := make([]types.ObjectIdentifier, 0, len(listed.Contents))
			unique := map[string]bool{}
			for _, object := range listed.Contents {
				if object.Key == nil || !strings.HasPrefix(*object.Key, b.Prefix) || unique[*object.Key] {
					return errDeleteBlocked
				}
				unique[*object.Key] = true
				keys = append(keys, types.ObjectIdentifier{Key: object.Key})
			}
			if len(keys) > 0 {
				if !remove {
					return errDeleteBlocked
				}
				// Guards are rechecked immediately before each destructive batch, not merely at inventory capture.
				if validateDeleteArchive(ctx, c, p, inv) != nil || checkDeleteProfile(ctx, c, p, inv) != nil || checkDeleteDependents(ctx, c, b.Server, inv) != nil || verifyDeleteStopped(ctx, c, inv) != nil {
					return errDeleteBlocked
				}
				deleted, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &b.Bucket, Delete: &types.Delete{Objects: keys, Quiet: aws.Bool(false)}})
				if err != nil || deleted == nil || len(deleted.Errors) != 0 || len(deleted.Deleted) != len(keys) {
					return errDeleteBlocked
				}
				for _, object := range deleted.Deleted {
					if object.Key == nil || !unique[*object.Key] || object.VersionId != nil || aws.ToBool(object.DeleteMarker) {
						return errDeleteBlocked
					}
					delete(unique, *object.Key)
				}
			}
			if listed.IsTruncated == nil {
				return errDeleteBlocked
			}
			if !*listed.IsTruncated {
				complete = true
				break
			}
			if listed.NextContinuationToken == nil || *listed.NextContinuationToken == "" || seenTokens[*listed.NextContinuationToken] {
				return errDeleteBlocked
			}
			token = listed.NextContinuationToken
			seenTokens[*token] = true
		}
		if !complete {
			return errDeleteBlocked
		}
	}
	return checkDeleteBucket(ctx, client, b.Bucket)
}
