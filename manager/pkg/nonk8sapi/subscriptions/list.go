// Copyright (c) 2022 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	set "github.com/deckarep/golang-set"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v4/pgxpool"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource/tableconvertor"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	appsv1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/apps/v1"

	"github.com/stolostron/multicluster-global-hub/manager/pkg/nonk8sapi/util"
)

const (
	serverInternalErrorMsg                      = "internal error"
	syncIntervalInSeconds                       = 4
	onlyPatchOfLabelsIsImplemented              = "only patch of labels is currently implemented"
	onlyAddOrRemoveAreImplemented               = "only add or remove operations are currently implemented"
	noRowsAffectedByOptimisticConcurrencyUpdate = "no rows were affected by an optimistic-concurrency update query"
	optimisticConcurrencyRetryAttempts          = 5
	crdName                                     = "subscriptions.apps.open-cluster-management.io"
)

var customResourceColumnDefinitions = util.GetCustomResourceColumnDefinitions(crdName,
	appsv1.SchemeGroupVersion.Version)

// ListSubscriptions middleware
func ListSubscriptions(dbConnectionPool *pgxpool.Pool) gin.HandlerFunc {
	return func(ginCtx *gin.Context) {
		labelSelector := ginCtx.Query("labelSelector")

		selectorInSql := ""

		if labelSelector != "" {
			var err error
			selectorInSql, err = util.ParseLabelSelector(labelSelector)
			if err != nil {
				fmt.Fprintf(gin.DefaultWriter, "failed to parse label selector: %s\n", err.Error())
				return
			}
		}

		fmt.Fprintf(gin.DefaultWriter, "parsed selector: %s\n", selectorInSql)

		limit := ginCtx.Query("limit")
		fmt.Fprintf(gin.DefaultWriter, "limit: %v\n", limit)

		lastSubscriptionName, lastSubscriptionUID := "", ""

		continueToken := ginCtx.Query("continue")
		if continueToken != "" {
			fmt.Fprintf(gin.DefaultWriter, "continue: %v\n", continueToken)

			var err error
			lastSubscriptionName, lastSubscriptionUID, err = util.DecodeContinue(continueToken)
			if err != nil {
				fmt.Fprintf(gin.DefaultWriter, "failed to decode continue token: %s\n", err.Error())
				return
			}
		}

		fmt.Fprintf(gin.DefaultWriter,
			"last returned subscription name: %s, last returned subscription UID: %s\n",
			lastSubscriptionName,
			lastSubscriptionUID)

		// build query condition for paging
		LastResourceCompareCondition := fmt.Sprintf(
			"(payload -> 'metadata' ->> 'name', payload -> 'metadata' ->> 'uid') > ('%s', '%s') ",
			lastSubscriptionName,
			lastSubscriptionUID)

		// the last subscription query order by subscription name and uid
		lastSubscriptionQuery := "SELECT payload FROM spec.subscriptions " +
			"ORDER BY (payload -> 'metadata' ->> 'name', payload -> 'metadata' ->> 'uid') DESC LIMIT 1"

		// subscrition list query
		subscriptionListQuery := "SELECT payload FROM spec.subscriptions WHERE deleted = FALSE AND " +
			LastResourceCompareCondition +
			selectorInSql +
			" ORDER BY (payload -> 'metadata' ->> 'name', payload -> 'metadata' ->> 'uid')"

		// add limit
		if limit != "" {
			subscriptionListQuery += fmt.Sprintf(" LIMIT %s", limit)
		}

		fmt.Fprintf(gin.DefaultWriter, "subscription list query: %v\n", subscriptionListQuery)

		if _, watch := ginCtx.GetQuery("watch"); watch {
			handleSubscriptionListForWatch(ginCtx, subscriptionListQuery, dbConnectionPool)
			return
		}

		handleRows(ginCtx, subscriptionListQuery, lastSubscriptionQuery, dbConnectionPool,
			customResourceColumnDefinitions)
	}
}

func handleSubscriptionListForWatch(ginCtx *gin.Context, subscriptionListQuery string,
	dbConnectionPool *pgxpool.Pool,
) {
	writer := ginCtx.Writer
	header := writer.Header()

	header.Set("Content-Type", "application/json")
	header.Set("Transfer-Encoding", "chunked")
	writer.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(syncIntervalInSeconds * time.Second)

	ctx, cancelContext := context.WithCancel(context.Background())
	defer cancelContext()

	preAddedSubscriptions := set.NewSet()

	for {
		select {
		case <-writer.CloseNotify():
			ticker.Stop()
			cancelContext()

			return
		case <-ticker.C:
			if ginCtx.Err() != nil || ginCtx.IsAborted() {
				ticker.Stop()
				cancelContext()

				return
			}

			doHandleRowsForWatch(ctx, writer, subscriptionListQuery, dbConnectionPool, preAddedSubscriptions)
		}
	}
}

func doHandleRowsForWatch(ctx context.Context, writer io.Writer, subscriptionListQuery string,
	dbConnectionPool *pgxpool.Pool, preAddedSubscriptions set.Set,
) {
	rows, err := dbConnectionPool.Query(ctx, subscriptionListQuery)
	if err != nil {
		fmt.Fprintf(gin.DefaultWriter, "error in quering subscription list: %v\n", err)
	}

	addedSubscriptions := set.NewSet()
	for rows.Next() {
		subscription := &appsv1.Subscription{}

		err := rows.Scan(subscription)
		if err != nil {
			continue
		}

		addedSubscriptions.Add(subscription.GetName())
		if err := util.SendWatchEvent(&metav1.WatchEvent{
			Type:   "ADDED",
			Object: runtime.RawExtension{Object: subscription},
		}, writer); err != nil {
			fmt.Fprintf(gin.DefaultWriter, "error in sending watch event: %v\n", err)
		}
	}

	subscriptionsToDelete := preAddedSubscriptions.Difference(addedSubscriptions)
	subscriptionsToDeleteIterator := subscriptionsToDelete.Iterator()
	for subscriptionToDelete := range subscriptionsToDeleteIterator.C {
		subscriptionToDeleteAsString, ok := subscriptionToDelete.(string)
		if !ok {
			continue
		}

		preAddedSubscriptions.Remove(subscriptionToDeleteAsString)

		subscriptionToDelete := &appsv1.Subscription{}
		subscriptionToDelete.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   appsv1.SchemeGroupVersion.Group,
			Version: appsv1.SchemeGroupVersion.Version,
			Kind:    "Subscription",
		})
		subscriptionToDelete.SetName(subscriptionToDeleteAsString)
		if err := util.SendWatchEvent(&metav1.WatchEvent{
			Type:   "DELETED",
			Object: runtime.RawExtension{Object: subscriptionToDelete},
		}, writer); err != nil {
			fmt.Fprintf(gin.DefaultWriter, "error in sending watch event: %v\n", err)
		}
	}

	subscriptionsToAdd := addedSubscriptions.Difference(preAddedSubscriptions)

	subscriptionsToAddIterator := subscriptionsToAdd.Iterator()
	for subscriptionToAdd := range subscriptionsToAddIterator.C {
		subscriptionToAddAsString, ok := subscriptionToAdd.(string)
		if !ok {
			continue
		}

		preAddedSubscriptions.Add(subscriptionToAddAsString)
	}

	writer.(http.Flusher).Flush()
}

func handleRows(ginCtx *gin.Context, subscriptionListQuery, lastSubscriptionQuery string,
	dbConnectionPool *pgxpool.Pool, customResourceColumnDefinitions []apiextensionsv1.CustomResourceColumnDefinition,
) {
	lastSubscription := &appsv1.Subscription{}
	if err := dbConnectionPool.QueryRow(context.TODO(), lastSubscriptionQuery).Scan(lastSubscription); err != nil {
		ginCtx.String(http.StatusInternalServerError, serverInternalErrorMsg)
		fmt.Fprintf(gin.DefaultWriter, "error in quering last subscription: %v\n", err)
		return
	}

	rows, err := dbConnectionPool.Query(context.TODO(), subscriptionListQuery)
	if err != nil {
		ginCtx.String(http.StatusInternalServerError, serverInternalErrorMsg)
		fmt.Fprintf(gin.DefaultWriter, "error in quering subscriptions: %v\n", err)
	}

	subscriptionList := &appsv1.SubscriptionList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "SubscriptionList",
			APIVersion: "apps.open-cluster-management.io/v1",
		},
		Items: []appsv1.Subscription{},
	}
	lastSubscriptionName, lastSubscriptionUID := "", ""
	for rows.Next() {
		subscription := appsv1.Subscription{}
		err := rows.Scan(&subscription)
		if err != nil {
			fmt.Fprintf(gin.DefaultWriter, "error in scanning a subscription: %v\n", err)
			continue
		}

		subscriptionList.Items = append(subscriptionList.Items, subscription)
		lastSubscriptionName = subscription.GetName()
		lastSubscriptionUID = string(subscription.GetUID())
	}

	if lastSubscriptionName != "" &&
		lastSubscriptionName != lastSubscription.GetName() &&
		lastSubscriptionUID != "" &&
		lastSubscriptionUID != string(lastSubscription.GetUID()) {
		continueToken, err := util.EncodeContinue(lastSubscriptionName, lastSubscriptionUID)
		if err != nil {
			ginCtx.String(http.StatusInternalServerError, serverInternalErrorMsg)
			fmt.Fprintf(gin.DefaultWriter, "error in encoding the continue token: %v\n", err)
			return
		}

		subscriptionList.SetContinue(continueToken)
	}

	if util.ShouldReturnAsTable(ginCtx) {
		fmt.Fprintf(gin.DefaultWriter, "Returning as table...\n")

		tableConvertor, err := tableconvertor.New(customResourceColumnDefinitions)
		if err != nil {
			fmt.Fprintf(gin.DefaultWriter, "error in creating table convertor: %v\n", err)
			return
		}

		subscriptionObjectList, err := wrapObjectsInList(subscriptionList.Items)
		if err != nil {
			fmt.Fprintf(gin.DefaultWriter, "error in wrapping subscriptions in a list: %v\n", err)
			return
		}

		table, err := tableConvertor.ConvertToTable(context.TODO(), subscriptionObjectList, nil)
		if err != nil {
			fmt.Fprintf(gin.DefaultWriter, "error in converting to table: %v\n", err)
			return
		}

		table.Kind = "Table"
		table.APIVersion = metav1.SchemeGroupVersion.String()
		ginCtx.JSON(http.StatusOK, table)

		return
	}

	ginCtx.JSON(http.StatusOK, subscriptionList)
}

func wrapObjectsInList(subscriptions []appsv1.Subscription) (*corev1.List, error) {
	list := &corev1.List{
		TypeMeta: metav1.TypeMeta{
			Kind:       "List",
			APIVersion: "v1",
		},
		ListMeta: metav1.ListMeta{},
	}

	for _, subscription := range subscriptions {
		// adopted from
		// https://github.com/kubernetes/kubectl/blob/4da03973dd2fcd4645f20ac669d8a73cb017ff39/pkg/cmd/get/get.go#L786
		subscriptionData, err := json.Marshal(subscription)
		if err != nil {
			return nil, fmt.Errorf("failed to marshall object: %w", err)
		}

		convertedObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, subscriptionData)
		if err != nil {
			return nil, fmt.Errorf("failed to decode with unstructured JSON scheme : %w", err)
		}

		list.Items = append(list.Items, runtime.RawExtension{Object: convertedObj})
	}

	return list, nil
}
