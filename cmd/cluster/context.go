package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	pd "github.com/PagerDuty/go-pagerduty"
	jira "github.com/andygrunwald/go-jira"
	"github.com/aws/aws-sdk-go/service/cloudtrail"
	"github.com/openshift-online/ocm-cli/pkg/dump"
	"github.com/openshift/osdctl/cmd/servicelog"
	sl "github.com/openshift/osdctl/internal/servicelog"
	"github.com/openshift/osdctl/pkg/osdCloud"
	"github.com/openshift/osdctl/pkg/osdctlConfig"
	"github.com/openshift/osdctl/pkg/printer"
	"github.com/openshift/osdctl/pkg/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

const (
	JiraTokenConfigKey            = "jira_token"
	JiraBaseURL                   = "https://issues.redhat.com"
	JiraTokenRegistrationPath     = "/secure/ViewProfile.jspa?selectedTab=com.atlassian.pats.pats-plugin:jira-user-personal-access-tokens"
	PagerDutyOauthTokenConfigKey  = "pd_oauth_token"
	PagerDutyUserTokenConfigKey   = "pd_user_token"
	PagerDutyTokenRegistrationUrl = "https://martindstone.github.io/PDOAuth/"
	PagerDutyTeamIDs              = "team_ids"
	shortOutputConfigValue        = "short"
	longOutputConfigValue         = "long"
	jsonOutputConfigValue         = "json"
)

type contextOptions struct {
	output            string
	verbose           bool
	full              bool
	clusterID         string
	externalClusterID string
	baseDomain        string
	organizationID    string
	days              int
	pages             int
	oauthtoken        string
	usertoken         string
	infraID           string
	awsProfile        string
	jiratoken         string
	team_ids          []string
}

type contextData struct {
	// Cluster info
	ClusterName    string
	ClusterVersion string
	ClusterID      string

	// Current OCM environment (e.g., "production" or "stage")
	OCMEnv string

	// limited Support Status
	LimitedSupportReasons []*cmv1.LimitedSupportReason
	// Service Logs
	ServiceLogs []sl.ServiceLogShort

	// Jira Cards
	JiraIssues        []jira.Issue
	SupportExceptions []jira.Issue

	// PD Alerts
	pdServiceID      []string
	PdAlerts         map[string][]pd.Incident
	HistoricalAlerts map[string][]*IncidentOccurrenceTracker

	// CloudTrail Logs
	CloudtrailEvents []*cloudtrail.Event
}
type IncidentOccurrenceTracker struct {
	IncidentName   string
	Count          int
	LastOccurrence string
}

// newCmdContext implements the context command to show the current context of a cluster
func newCmdContext() *cobra.Command {
	ops := newContextOptions()
	contextCmd := &cobra.Command{
		Use:               "context",
		Short:             "Shows the context of a specified cluster",
		Args:              cobra.ExactArgs(1),
		DisableAutoGenTag: true,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(ops.complete(cmd, args))
			cmdutil.CheckErr(ops.run())
		},
	}

	contextCmd.Flags().StringVarP(&ops.output, "output", "o", "long", "Valid formats are ['long', 'short', 'json']. Output is set to 'long' by default")
	contextCmd.Flags().StringVarP(&ops.clusterID, "cluster-id", "C", "", "Cluster ID")
	contextCmd.Flags().StringVarP(&ops.awsProfile, "profile", "p", "", "AWS Profile")
	contextCmd.Flags().BoolVarP(&ops.verbose, "verbose", "", false, "Verbose output")
	contextCmd.Flags().BoolVar(&ops.full, "full", false, "Run full suite of checks.")
	contextCmd.Flags().IntVarP(&ops.days, "days", "d", 30, "Command will display X days of Error SLs sent to the cluster. Days is set to 30 by default")
	contextCmd.Flags().IntVar(&ops.pages, "pages", 40, "Command will display X pages of Cloud Trail logs for the cluster. Pages is set to 40 by default")
	contextCmd.Flags().StringVar(&ops.oauthtoken, "oauthtoken", "", fmt.Sprintf("Pass in PD oauthtoken directly. If not passed in, by default will read `pd_oauth_token` from ~/.config/%s.\nPD OAuth tokens can be generated by visiting %s", osdctlConfig.ConfigFileName, PagerDutyTokenRegistrationUrl))
	contextCmd.Flags().StringVar(&ops.usertoken, "usertoken", "", fmt.Sprintf("Pass in PD usertoken directly. If not passed in, by default will read `pd_user_token` from ~/config/%s", osdctlConfig.ConfigFileName))
	contextCmd.Flags().StringVar(&ops.jiratoken, "jiratoken", "", fmt.Sprintf("Pass in the Jira access token directly. If not passed in, by default will read `jira_token` from ~/.config/%s.\nJira access tokens can be registered by visiting %s/%s", osdctlConfig.ConfigFileName, JiraBaseURL, JiraTokenRegistrationPath))
	contextCmd.Flags().StringArrayVarP(&ops.team_ids, "team-ids", "t", []string{}, fmt.Sprintf("Pass in PD teamids directly to filter the PD Alerts by team. Can also be defined as `team_ids` in ~/.config/%s\nWill show all PD Alerts for all PD service IDs if none is defined", osdctlConfig.ConfigFileName))
	return contextCmd
}

func newContextOptions() *contextOptions {
	return &contextOptions{}
}

func (o *contextOptions) complete(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return cmdutil.UsageErrorf(cmd, "Provide exactly one cluster ID")
	}

	if o.days < 1 {
		return fmt.Errorf("cannot have a days value lower than 1")
	}

	// Create OCM client to talk to cluster API
	ocmClient := utils.CreateConnection()
	defer func() {
		if err := ocmClient.Close(); err != nil {
			fmt.Printf("Cannot close the ocmClient (possible memory leak): %q", err)
		}
	}()

	clusters := utils.GetClusters(ocmClient, args)
	if len(clusters) != 1 {
		return fmt.Errorf("unexpected number of clusters matched input. Expected 1 got %d", len(clusters))
	}

	cluster := clusters[0]
	o.clusterID = cluster.ID()
	o.externalClusterID = cluster.ExternalID()
	o.baseDomain = cluster.DNS().BaseDomain()
	o.infraID = cluster.InfraID()

	orgID, err := utils.GetOrgfromClusterID(ocmClient, *cluster)
	if err != nil {
		fmt.Printf("Failed to get Org ID for cluster ID %s - err: %q", o.clusterID, err)
		o.organizationID = ""
	} else {
		o.organizationID = orgID
	}

	return nil
}

func (o *contextOptions) run() error {

	var printFunc func(*contextData)
	switch o.output {
	case shortOutputConfigValue:
		printFunc = o.printShortOutput
	case longOutputConfigValue:
		printFunc = o.printLongOutput
	case jsonOutputConfigValue:
		printFunc = o.printJsonOutput
	default:
		return fmt.Errorf("Unknown Output Format: %s", o.output)
	}

	currentData, dataErrors := o.generateContextData()
	if currentData == nil {
		fmt.Fprintf(os.Stderr, "Failed to query cluster info: %+v", dataErrors)
		os.Exit(1)
	}

	if len(dataErrors) > 0 {
		fmt.Fprintf(os.Stderr, "Encountered Errors during data collection. Displayed data may be incomplete: \n")
		for _, dataError := range dataErrors {
			fmt.Fprintf(os.Stderr, "\t%v\n", dataError)
		}
	}

	printFunc(currentData)

	return nil
}

func (o *contextOptions) printLongOutput(data *contextData) {

	printClusterInfo(o.clusterID)

	printSupportStatus(data.LimitedSupportReasons)

	// Print the Servicelogs for this cluster
	printServiceLogs(data.ServiceLogs, o.verbose, o.days)

	printJIRAOHSS(data.JiraIssues)

	printSupportStatus(data.LimitedSupportReasons)

	printCurrentPDAlerts(data.PdAlerts, data.pdServiceID)

	if o.full {
		printHistoricalPDAlertSummary(data.HistoricalAlerts, data.pdServiceID, o.days)

		err := printCloudTrailLogs(data.CloudtrailEvents)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Can't print cloudtrail: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Println()
		fmt.Println("============================================================")
		fmt.Println("CloudTrail events for the Cluster")
		fmt.Println("============================================================")
		fmt.Println("Not polling cloudtrail logs, use --full flag to do so (must be logged into the correct hive to work).")
	}

	// Print other helpful links
	err := o.printOtherLinks(data.OCMEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't print other links: %v\n", err)
	}
}
func (o *contextOptions) printShortOutput(data *contextData) {

	fmt.Println()
	fmt.Println("============================================================")
	fmt.Printf("%s -- %s\n", data.ClusterName, data.ClusterID)
	fmt.Println("============================================================")
	fmt.Println()

	highAlertCount := 0
	lowAlertCount := 0
	for _, alerts := range data.PdAlerts {
		for _, alert := range alerts {
			if strings.ToLower(alert.Urgency) == "high" {
				highAlertCount++
			} else {
				lowAlertCount++
			}
		}
	}

	historicalAlertsString := "N/A"
	historicalAlertsCount := 0
	if data.HistoricalAlerts != nil {
		for _, histAlerts := range data.HistoricalAlerts {
			for _, histAlert := range histAlerts {
				historicalAlertsCount += histAlert.Count
			}
		}
		historicalAlertsString = fmt.Sprintf("%d", historicalAlertsCount)
	}

	table := printer.NewTablePrinter(os.Stdout, 20, 1, 2, ' ')
	table.AddRow([]string{
		"Version",
		"Supported?",
		fmt.Sprintf("SLs (last %d d)", o.days),
		"Jira Tickets",
		"Current Alerts",
		fmt.Sprintf("Historical Alerts (last %d d)", o.days),
	})
	table.AddRow([]string{
		data.ClusterVersion,
		fmt.Sprintf("%t", len(data.LimitedSupportReasons) == 0),
		fmt.Sprintf("%d", len(data.ServiceLogs)),
		fmt.Sprintf("%d", len(data.JiraIssues)),
		fmt.Sprintf("H: %d | L: %d", highAlertCount, lowAlertCount),
		historicalAlertsString,
	})

	table.Flush()
}

func (o *contextOptions) printJsonOutput(data *contextData) {
	jsonout, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		fmt.Printf("Can't marshal results to json: %v\n", err)
		return
	}

	fmt.Println(string(jsonout))
}

// generateContextData Creates a contextData struct that contains all the
// cluster context information requested by the contextOptions. if a certain
// datapoint can not be queried, the appropriate field will be null and the
// errors array will contain information about the error. The first return
// value will only be nil, if this function fails to get basic cluster
// information. The second return value will *never* be nil, but instead have a
// lenght of 0 if no errors occured
func (o *contextOptions) generateContextData() (*contextData, []error) {
	data := &contextData{}
	errors := []error{}

	ocmClient := utils.CreateConnection()
	defer ocmClient.Close()
	cluster, err := utils.GetCluster(ocmClient, o.clusterID)

	if err != nil {
		errors = append(errors, err)
		return nil, errors
	}

	data.ClusterName = cluster.Name()
	data.ClusterID = cluster.ID()
	data.ClusterVersion = cluster.Version().RawID()
	data.OCMEnv = utils.GetCurrentOCMEnv(ocmClient)

	fmt.Fprintln(os.Stderr, "Getting Limited Support Reason...")
	limitedSupportReasons, err := utils.GetClusterLimitedSupportReasons(ocmClient, cluster.ID())
	if err != nil {
		errors = append(errors, fmt.Errorf("Error while getting Limited Support status reasons: %v", err))
	} else {
		data.LimitedSupportReasons = append(data.LimitedSupportReasons, limitedSupportReasons...)
	}

	fmt.Fprintln(os.Stderr, "Getting Service Logs...")
	data.ServiceLogs, err = GetServiceLogsSince(cluster.ID(), o.days)
	if err != nil {
		errors = append(errors, fmt.Errorf("Error while getting the service logs: %v", err))
	}

	fmt.Fprintln(os.Stderr, "Getting Jira Issues...")
	data.JiraIssues, err = GetJiraIssuesForCluster(o.clusterID, o.externalClusterID)
	if err != nil {
		errors = append(errors, fmt.Errorf("Error while getting the open jira tickets: %v", err))
	}

	fmt.Fprintln(os.Stderr, "Getting Support Exceptions...")
	data.SupportExceptions, err = GetJiraSupportExceptionsForOrg(o.organizationID)
	if err != nil {
		errors = append(errors, fmt.Errorf("Error while getting support exceptions: %v", err))
	}

	fmt.Fprintln(os.Stderr, "Getting Pagerduty Service...")
	data.pdServiceID, err = GetPDServiceID(o.baseDomain, o.usertoken, o.oauthtoken, o.team_ids)
	if err != nil {
		errors = append(errors, fmt.Errorf("Error getting PD Service ID: %v", err))
	}

	fmt.Fprintln(os.Stderr, "Getting current Pagerduty Alerts...")
	data.PdAlerts, err = GetCurrentPDAlertsForCluster(data.pdServiceID, o.usertoken, o.oauthtoken)
	if err != nil {
		errors = append(errors, fmt.Errorf("Error while getting current PD Alerts: %v", err))
	}

	if o.full {
		fmt.Fprintln(os.Stderr, "Getting historical Pagerduty Alerts...")
		data.HistoricalAlerts, err = GetHistoricalPDAlertsForCluster(data.pdServiceID, o.usertoken, o.oauthtoken)
		if err != nil {
			errors = append(errors, fmt.Errorf("Error while getting historical PD Alert Data: %v", err))
		}
		fmt.Fprintln(os.Stderr, "Getting Cloudtrail events...")
		data.CloudtrailEvents, err = GetCloudTrailLogsForCluster(o.awsProfile, o.clusterID, o.pages)
		if err != nil {
			errors = append(errors, fmt.Errorf("Error getting cloudtrail logs for cluster: %v", err))
		}
	}

	return data, errors
}

func GetCurrentPDAlertsForCluster(pdServiceIDs []string, pdUsertoken string, pdAuthtoken string) (map[string][]pd.Incident, error) {
	fmt.Fprintln(os.Stderr, "Getting currently firing Pagerduty Alerts for the cluster.")
	pdClient, err := GetPagerdutyClient(pdUsertoken, pdAuthtoken)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error getting pd client: ", err.Error())
		return nil, err
	}
	incidents := map[string][]pd.Incident{}

	var incidentLimit uint = 25
	var incdientListOffset uint = 0
	for _, pdServiceID := range pdServiceIDs {
		for {
			listIncidentsResponse, err := pdClient.ListIncidentsWithContext(
				context.TODO(),
				pd.ListIncidentsOptions{
					ServiceIDs: []string{pdServiceID},
					Statuses:   []string{"triggered", "acknowledged"},
					SortBy:     "urgency:DESC",
					Limit:      incidentLimit,
					Offset:     incdientListOffset,
				},
			)
			if err != nil {
				return nil, err
			}

			incidents[pdServiceID] = append(incidents[pdServiceID], listIncidentsResponse.Incidents...)

			if !listIncidentsResponse.More {
				break
			}
			incdientListOffset += incidentLimit
		}
	}
	return incidents, nil
}

func GetHistoricalPDAlertsForCluster(pdServiceIDs []string, pdUsertoken string, pdAuthtoken string) (map[string][]*IncidentOccurrenceTracker, error) {

	var currentOffset uint
	var limit uint = 100
	var incidents []pd.Incident
	var ctx context.Context = context.TODO()
	incidentmap := map[string][]*IncidentOccurrenceTracker{}

	pdClient, err := GetPagerdutyClient(pdUsertoken, pdAuthtoken)
	if err != nil {
		fmt.Println("error getting pd client: ", err.Error())
		return nil, err
	}

	for _, pdServiceID := range pdServiceIDs {
		for currentOffset = 0; true; currentOffset += limit {
			liResponse, err := pdClient.ListIncidentsWithContext(
				ctx,
				pd.ListIncidentsOptions{
					ServiceIDs: []string{pdServiceID},
					Statuses:   []string{"resolved", "triggered", "acknowledged"},
					Offset:     currentOffset,
					Limit:      limit,
					SortBy:     "created_at:desc",
				},
			)

			if err != nil {
				return nil, err
			}

			if len(liResponse.Incidents) == 0 {
				break
			}

			incidents = append(incidents, liResponse.Incidents...)
		}
		incidentCounter := make(map[string]*IncidentOccurrenceTracker)

		for _, incident := range incidents {
			title := strings.Split(incident.Title, " ")[0]
			if _, found := incidentCounter[title]; found {
				incidentCounter[title].Count++

				// Compare current incident timestamp vs our previous 'latest occurrence', and save the most recent.
				currentLastOccurence, err := time.Parse(time.RFC3339, incidentCounter[title].LastOccurrence)
				if err != nil {
					fmt.Printf("Failed to parse time %q\n", err)
					return nil, err
				}

				incidentCreatedAt, err := time.Parse(time.RFC3339, incident.CreatedAt)
				if err != nil {
					fmt.Printf("Failed to parse time %q\n", err)
					return nil, err
				}

				// We want to see when the latest occurrence was
				if incidentCreatedAt.After(currentLastOccurence) {
					incidentCounter[title].LastOccurrence = incident.CreatedAt
				}

			} else {
				// First time encountering this incident type
				incidentCounter[title] = &IncidentOccurrenceTracker{
					IncidentName:   title,
					Count:          1,
					LastOccurrence: incident.CreatedAt,
				}
			}
		}

		var incidentSlice []*IncidentOccurrenceTracker = make([]*IncidentOccurrenceTracker, 0, len(incidentCounter))
		for _, val := range incidentCounter {
			incidentSlice = append(incidentSlice, val)
		}

		sort.Slice(incidentSlice, func(i int, j int) bool {
			return incidentSlice[i].Count < incidentSlice[j].Count
		})
		incidentmap[pdServiceID] = append(incidentmap[pdServiceID], incidentSlice...)

	}

	return incidentmap, nil

}

// GetJiraClient creates a jira client that connects to
// https://issues.redhat.com. To work, the jiraToken needs to be set in the
// config
func GetJiraClient() (*jira.Client, error) {
	if !viper.IsSet(JiraTokenConfigKey) {
		return nil, fmt.Errorf("key %s is not set in config file", JiraTokenConfigKey)
	}

	jiratoken := viper.GetString(JiraTokenConfigKey)

	tp := jira.PATAuthTransport{
		Token: jiratoken,
	}
	return jira.NewClient(tp.Client(), JiraBaseURL)
}

func GetJiraIssuesForCluster(clusterID string, externalClusterID string) ([]jira.Issue, error) {

	jiraClient, err := GetJiraClient()
	if err != nil {
		return nil, fmt.Errorf("Error connecting to jira: %v", err)
	}

	jql := fmt.Sprintf(
		`(project = "OpenShift Hosted SRE Support" AND "Cluster ID" ~ "%s") 
		OR (project = "OpenShift Hosted SRE Support" AND "Cluster ID" ~ "%s") 
		ORDER BY created DESC`,
		externalClusterID,
		clusterID,
	)

	issues, _, err := jiraClient.Issue.Search(jql, nil)
	if err != nil {
		fmt.Printf("Failed to search for jira issues %q\n", err)
		return nil, err
	}

	return issues, nil
}

func GetJiraSupportExceptionsForOrg(organizationID string) ([]jira.Issue, error) {
	jiraClient, err := GetJiraClient()
	if err != nil {
		return nil, fmt.Errorf("Error connecting to jira: %v", err)
	}

	jql := fmt.Sprintf(
		`project = "Support Exceptions" AND type = Story AND Status = Approved AND
		 Resolution = Unresolved AND "Customer Name" ~ "%s"`,
		organizationID,
	)

	issues, _, err := jiraClient.Issue.Search(jql, nil)
	if err != nil {
		fmt.Printf("Failed to search for jira issues %q\n", err)
		return nil, err
	}

	return issues, nil
}

// GetServiceLogsSince returns the Servicelogs for a cluster sent between
// time.Now() and time.Now()-duration. the first parameter will contain a slice
// of the servicelogs from the given time period, while the second return value
// indicates if an error has happened.
func GetServiceLogsSince(clusterID string, days int) ([]sl.ServiceLogShort, error) {

	// time.Now().Sub() returns the duration between two times, so we negate the duration in Add()
	earliestTime := time.Now().AddDate(0, 0, -days)

	slResponse, err := servicelog.FetchServiceLogs(clusterID)
	if err != nil {
		return nil, err
	}

	var serviceLogs sl.ServiceLogShortList
	err = json.Unmarshal(slResponse.Bytes(), &serviceLogs)
	if err != nil {
		fmt.Printf("Failed to unmarshal the SL response %q\n", err)
		return nil, err
	}

	// Parsing the relevant servicelogs
	// - We only care about SLs sent in the given duration
	var errorServiceLogs []sl.ServiceLogShort
	for _, serviceLog := range serviceLogs.Items {
		if serviceLog.CreatedAt.After(earliestTime) {
			errorServiceLogs = append(errorServiceLogs, serviceLog)
		}
	}

	return errorServiceLogs, nil
}

// Returns an empty array if team_ids has not been informed via CLI or config file
// that will make the query show all PD Alerts for all PD services by default
func getPDTeamIDs(team_ids []string) []string {
	if len(team_ids) == 0 {
		if !viper.IsSet(PagerDutyTeamIDs) {
			return []string{}
		}
		team_ids = viper.GetStringSlice(PagerDutyTeamIDs)
	}
	return team_ids
}

func getPDUserClient(usertoken string) (*pd.Client, error) {
	if usertoken == "" {
		if !viper.IsSet(PagerDutyUserTokenConfigKey) {
			return nil, fmt.Errorf("key %s is not set in config file", PagerDutyUserTokenConfigKey)
		}
		usertoken = viper.GetString(PagerDutyUserTokenConfigKey)
	}
	return pd.NewClient(usertoken), nil
}

func getPDOauthClient(oauthtoken string) (*pd.Client, error) {
	if oauthtoken == "" {
		if !viper.IsSet(PagerDutyOauthTokenConfigKey) {
			return nil, fmt.Errorf("key %s is not set in config file", PagerDutyOauthTokenConfigKey)
		}
		oauthtoken = viper.GetString(PagerDutyOauthTokenConfigKey)
	}
	return pd.NewOAuthClient(oauthtoken), nil
}

func GetPagerdutyClient(usertoken string, oauthtoken string) (*pd.Client, error) {
	client, err := getPDUserClient(usertoken)
	if client != nil {
		return client, err
	}

	client, err = getPDOauthClient(oauthtoken)
	if err != nil {
		return nil, fmt.Errorf("failed to create both user and oauth clients for pd, neither key pd_oauth_token or pd_user_token are set in config file")
	}
	return client, err
}

func GetPDServiceID(baseDomain string, usertoken string, oauthtoken string, team_ids []string) ([]string, error) {

	pdClient, err := GetPagerdutyClient(usertoken, oauthtoken)
	if err != nil {
		return nil, fmt.Errorf("failed to GetPagerdutyClient: %w", err)
	}

	// Gets the PD Team IDS
	teams := getPDTeamIDs(team_ids)

	lsResponse, err := pdClient.ListServicesWithContext(context.TODO(), pd.ListServiceOptions{Query: baseDomain, TeamIDs: teams})

	if err != nil {
		fmt.Printf("Failed to ListServicesWithContext %q\n", err)
		return []string{}, err
	}

	serviceIDS := []string{}
	for _, service := range lsResponse.Services {
		serviceIDS = append(serviceIDS, service.ID)
	}

	return serviceIDS, nil
}

func GetCloudTrailLogsForCluster(awsProfile string, clusterID string, maxPages int) ([]*cloudtrail.Event, error) {
	fmt.Fprintln(os.Stderr, "Pulling and filtering the past", maxPages, "pages of Cloudtrail data")

	awsJumpClient, err := osdCloud.GenerateAWSClientForCluster(awsProfile, clusterID)
	if err != nil {
		return nil, err
	}

	foundEvents := []*cloudtrail.Event{}

	var eventSearchInput = cloudtrail.LookupEventsInput{}

	for counter := 0; counter <= maxPages; counter++ {
		print(".")
		cloudTrailEvents, err := awsJumpClient.LookupEvents(&eventSearchInput)
		if err != nil {
			return nil, err
		}

		foundEvents = append(foundEvents, cloudTrailEvents.Events...)

		// for pagination
		eventSearchInput.NextToken = cloudTrailEvents.NextToken
		if cloudTrailEvents.NextToken == nil {
			break
		}
	}
	filteredEvents := []*cloudtrail.Event{}
	for _, event := range foundEvents {
		if skippableEvent(*event.EventName) {
			continue
		}
		if event.Username != nil && strings.Contains(*event.Username, "RH-SRE-") {
			continue
		}
		filteredEvents = append(filteredEvents, event)
	}

	return filteredEvents, nil
}

func printClusterInfo(clusterID string) {

	fmt.Println("============================================================")
	fmt.Println("Cluster Info")
	fmt.Println("============================================================")

	cmd := "ocm describe cluster " + clusterID
	output, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, string(output))
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Println(string(output))
}

func printServiceLogs(serviceLogs []sl.ServiceLogShort, verbose bool, sinceDays int) {

	fmt.Println("============================================================")
	fmt.Println("Service Logs sent in the past", sinceDays, "Days")
	fmt.Println("============================================================")

	if verbose {
		marshalledSLs, err := json.MarshalIndent(serviceLogs, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't prepare service logs for printing: %v", err)
		}
		dump.Pretty(os.Stdout, marshalledSLs)
	} else {
		// Non verbose only prints the summaries
		for i, errorServiceLog := range serviceLogs {
			fmt.Printf("%d. %s (%s)\n", i, errorServiceLog.Summary, errorServiceLog.CreatedAt.Format(time.RFC3339))
		}
	}
	fmt.Println()
}

// printSupportStatus reports if a cluster is in limited support or fully supported.
func printSupportStatus(limitedSupportReasons []*cmv1.LimitedSupportReason) {

	fmt.Println("============================================================")
	fmt.Println("Limited Support Status")
	fmt.Println("============================================================")

	// No reasons found, cluster is fully supported
	if len(limitedSupportReasons) == 0 {
		fmt.Printf("Cluster is fully supported\n")
		fmt.Println()
		return
	}

	table := printer.NewTablePrinter(os.Stdout, 20, 1, 3, ' ')
	table.AddRow([]string{"Reason ID", "Summary", "Details"})
	for _, clusterLimitedSupportReason := range limitedSupportReasons {
		table.AddRow([]string{clusterLimitedSupportReason.ID(), clusterLimitedSupportReason.Summary(), clusterLimitedSupportReason.Details()})
	}
	// Add empty row for readability
	table.AddRow([]string{})
	if err := table.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error printing Support Status: %v", err)
	}
}

func printCurrentPDAlerts(incidents map[string][]pd.Incident, serviceIDs []string) {

	fmt.Println("============================================================")
	fmt.Println("Current Pagerduty Alerts for the Cluster")
	fmt.Println("============================================================")
	for _, ID := range serviceIDs {
		fmt.Printf("Link to PD Service: https://redhat.pagerduty.com/service-directory/%s\n", ID)
		table := printer.NewTablePrinter(os.Stdout, 20, 1, 3, ' ')
		table.AddRow([]string{"Urgency", "Title", "Created At"})
		for _, incident := range incidents[ID] {
			table.AddRow([]string{incident.Urgency, incident.Title, incident.CreatedAt})
		}
		// Add empty row for readability
		table.AddRow([]string{})
		err := table.Flush()
		if err != nil {
			fmt.Println("error while flushing table: ", err.Error())
			return
		}
	}

}

func printHistoricalPDAlertSummary(incidentCounters map[string][]*IncidentOccurrenceTracker, serviceIDs []string, sinceDays int) error {

	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("Historical Pagerduty Alert Summary")
	fmt.Println("============================================================")
	for _, serviceID := range serviceIDs {
		fmt.Printf("Link to PD Service: https://redhat.pagerduty.com/service-directory/%s\n", serviceID)
		println()

		table := printer.NewTablePrinter(os.Stdout, 20, 1, 3, ' ')
		table.AddRow([]string{"Type", "Count", "Last Occurrence"})

		totalIncidents := 0
		for _, incident := range incidentCounters[serviceID] {
			table.AddRow([]string{incident.IncidentName, strconv.Itoa(incident.Count), incident.LastOccurrence})
			totalIncidents += incident.Count
		}

		// Add empty row for readability
		table.AddRow([]string{})
		err := table.Flush()
		if err != nil {
			fmt.Println("error while flushing table: ", err.Error())
			return err

		}
		fmt.Println("Total number of incidents [", totalIncidents, "] in [", sinceDays, "] days")
		fmt.Println()
	}

	return nil
}

func printJIRAOHSS(issues []jira.Issue) {

	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("Cluster OHSS Cards")
	fmt.Println("============================================================")

	for _, i := range issues {
		fmt.Printf("[%s](%s/%s): %+v\n", i.Key, i.Fields.Type.Name, i.Fields.Priority.Name, i.Fields.Summary)
		fmt.Printf("- Created: %s\tStatus: %s\n", time.Time(i.Fields.Created).Format("2006-01-02 15:04"), i.Fields.Status.Name)
		fmt.Printf("- Link: %s/browse/%s\n\n", JiraBaseURL, i.Key)
	}

	if len(issues) == 0 {
		fmt.Println("No OHSS Cards found")
		fmt.Println()
	}
}

func printJIRASupportExceptions(issues []jira.Issue) {

	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("Cluster Org Support Exception")
	fmt.Println("============================================================")
	for _, i := range issues {
		fmt.Printf("[%s](%s/%s): %+v [Status: %s]\n", i.Key, i.Fields.Type.Name, i.Fields.Priority.Name, i.Fields.Summary, i.Fields.Status.Name)
		fmt.Printf("- Link: %s/browse/%s\n\n", JiraBaseURL, i.Key)
	}

	if len(issues) == 0 {
		fmt.Println("No Support Exceptions found")
		fmt.Println()
	}
}

func (o *contextOptions) printOtherLinks(OCMEnv string) error {
	fmt.Println("============================================================")
	fmt.Println("External resources containing related cluster data")
	fmt.Println("============================================================")
	// Determine whether to use the prod or stage Splunk index
	splunkIndex := "openshift_managed_audit"
	if OCMEnv == "stage" {
		splunkIndex = "openshift_managed_audit_stage"
	}
	// Clusters in integration don't forward to splunk
	if OCMEnv != "integration" {
		fmt.Printf("Link to Splunk audit logs (set time in Splunk): https://osdsecuritylogs.splunkcloud.com/en-US/app/search/search?q=search%%20index%%3D%%22%s%%22%%20clusterid%%3D%%22%s%%22\n\n", splunkIndex, o.infraID)
	}
	fmt.Printf("Link to OHSS tickets: %s/issues/?jql=project%%20%%3D%%20OHSS%%20and%%20(%%22Cluster%%20ID%%22%%20~%%20%%20%%22%s%%22%%20OR%%20%%22Cluster%%20ID%%22%%20~%%20%%22%s%%22)\n\n", JiraBaseURL, o.clusterID, o.externalClusterID)
	fmt.Printf("Link to CCX dashboard: https://kraken.psi.redhat.com/clusters/%s\n\n", o.externalClusterID)

	return nil
}

func printCloudTrailLogs(events []*cloudtrail.Event) error {
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("Potentially interesting CloudTrail events for the Cluster")
	fmt.Println("============================================================")

	table := printer.NewTablePrinter(os.Stdout, 20, 1, 3, ' ')
	table.AddRow([]string{"EventId", "EventName", "Username", "EventTime"})
	for _, event := range events {
		if event.Username == nil {
			table.AddRow([]string{*event.EventId, *event.EventName, "", event.EventTime.String()})
		} else {
			table.AddRow([]string{*event.EventId, *event.EventName, *event.Username, event.EventTime.String()})
		}
	}
	// Add empty row for readability
	table.AddRow([]string{})
	return table.Flush()
}

// These are a list of skippable aws event types, as they won't indicate any modification on the customer's side.
func skippableEvent(eventName string) bool {
	skippableList := []string{
		"Get",
		"List",
		"Describe",
		"AssumeRole",
		"Encrypt",
		"Decrypt",
		"LookupEvents",
		"GenerateDataKey",
	}

	for _, skipword := range skippableList {
		if strings.Contains(eventName, skipword) {
			return true
		}
	}
	return false
}
