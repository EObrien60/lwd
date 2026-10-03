package cli

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"lwd/internal/store"
)

// --- databases and backups (M2) ---

func (c *cli) dbStatus(args []string) error {
	fs := newFlags("db status")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	rs, err := cl.Resources(c.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	var last *store.Backup
	for _, r := range rs {
		if r.Kind != store.ResourceDatabase {
			continue
		}
		bs, err := cl.DBBackups(c.ctx, pos[0], pos[1])
		if err != nil {
			return err
		}
		for i := range bs {
			if bs[i].Status == store.BackupSucceeded {
				last = &bs[i]
				break
			}
		}
	}
	if *asJSON {
		return c.printJSON(map[string]any{"resources": rs, "last_backup": last})
	}
	if len(rs) == 0 {
		fmt.Fprintf(c.out, "%s/%s has no resources (declare database = true or storage = true and deploy)\n", pos[0], pos[1])
		return nil
	}
	c.table("KIND\tNAME\tHOST\tCREATED", func(w io.Writer) {
		for _, r := range rs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Kind, r.Name, r.Host, ts(r.CreatedAt))
		}
	})
	if last != nil {
		fmt.Fprintf(c.out, "last backup: %d %s (%s, %d bytes)\n", last.ID, last.File, ts(last.StartedAt), last.Bytes)
	} else {
		for _, r := range rs {
			if r.Kind == store.ResourceDatabase {
				fmt.Fprintln(c.out, "last backup: none")
			}
		}
	}
	return nil
}

func (c *cli) dbBackup(args []string) error {
	fs := newFlags("db backup")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	b, err := cl.BackupDB(c.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(b)
	}
	fmt.Fprintf(c.out, "backup %d of %s on %s: %s (%d bytes, sha256 %s)\n", b.ID, b.Database, b.Host, b.File, b.Bytes, b.SHA256)
	return nil
}

func (c *cli) dbBackups(args []string) error {
	fs := newFlags("db backups")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	bs, err := cl.DBBackups(c.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(bs)
	}
	c.table("ID\tKIND\tSTATUS\tSTARTED\tFILE\tBYTES\tERROR", func(w io.Writer) {
		for _, b := range bs {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%d\t%s\n", b.ID, b.Kind, b.Status, ts(b.StartedAt), dash(b.File), b.Bytes, oneLine(b.Error))
		}
	})
	return nil
}

func (c *cli) dbRestore(args []string) error {
	fs := newFlags("db restore")
	yes := fs.Bool("yes", false, "confirm: the database is replaced by the backup")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args, 3, 3)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(pos[2], 10, 64)
	if err != nil || id <= 0 {
		return usagef("BACKUP_ID must be a backup id (see: lwd db backups %s %s)", pos[0], pos[1])
	}
	if !*yes {
		return usagef("restore replaces the %s/%s database with backup %d and stops the app meanwhile; rerun with --yes", pos[0], pos[1], id)
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	fmt.Fprintf(c.err, "restoring backup %d into %s/%s...\n", id, pos[0], pos[1])
	r, err := cl.RestoreDB(c.ctx, pos[0], pos[1], id)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(r)
	}
	ms := func(n int64) string { return (time.Duration(n) * time.Millisecond).String() }
	fmt.Fprintf(c.out, "restored backup %d (%s) into %s in %s (stop %s, restore %s, start %s)\n",
		r.Backup.ID, r.Backup.File, r.Backup.Database, ms(r.TotalMS), ms(r.StopMS), ms(r.RestoreMS), ms(r.StartMS))
	return nil
}

func (c *cli) backupStatus(args []string) error {
	fs := newFlags("backup status")
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	st, err := cl.BackupStatus(c.ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.printJSON(st)
	}
	unhealthy := false
	c.table("APP/ENV\tDATABASE\tHOST\tLATEST BACKUP\tFILE\tLAST FAILURE", func(w io.Writer) {
		for _, d := range st.Databases {
			latest, file, failure := "never", "-", "-"
			if d.Latest != nil {
				latest, file = ts(d.Latest.StartedAt), d.Latest.File
			} else {
				unhealthy = true
			}
			if d.LastFailure != nil {
				failure = ts(d.LastFailure.StartedAt) + " " + oneLine(d.LastFailure.Error)
				unhealthy = true
			}
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\t%s\n", d.App, d.Env, d.Database, d.Host, latest, file, failure)
		}
	})
	if len(st.Failures) > 0 {
		fmt.Fprintln(c.out, "\nFAILURES (last 7 days)")
		c.table("ID\tAPP/ENV\tKIND\tSTARTED\tERROR", func(w io.Writer) {
			for _, b := range st.Failures {
				fmt.Fprintf(w, "%d\t%s/%s\t%s\t%s\t%s\n", b.ID, b.App, b.Env, b.Kind, ts(b.StartedAt), oneLine(b.Error))
			}
		})
	}
	if unhealthy {
		return errReported
	}
	return nil
}
